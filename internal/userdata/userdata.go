// SPDX-License-Identifier: AGPL-3.0-or-later

// Package userdata is what a user may take with them and what they may have erased (audit finding PRIV-05): an export of the
// personal data the controller holds about one user, and the part of an erasure that is plain SQL. The handsets, SIP accounts and
// base stations of the user are removed by their own stores first (they tell the cells and the SIP side); this package finishes.
package userdata

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
)

// ErrLastAdmin is returned when erasing the only administrator.
var ErrLastAdmin = errors.New("userdata: this is the only administrator; make another one first")

// maxRows bounds each list of the export (the newest come first).
const maxRows = 20000

// Export returns the user's data as indented JSON. It holds no secrets: no password hash, no handset key, no SIP password, no
// two-factor secret. It does hold what the user did and had: handsets, VoIP lines (without passwords), contacts, their calls with the
// remote numbers, the base stations they enrolled, organizations, and the audit entries made by them.
func Export(ctx context.Context, pool *pgxpool.Pool, user uuid.UUID) ([]byte, error) {
	sections := []struct{ name, query string }{
		{"profile", `SELECT username, display_name, email, role, auth_source, created_at, last_login_at, trust_scope, email_verified
			FROM app_user WHERE id = $1`},
		{"handsets", `SELECT h.id, h.display_name, h.model, h.profile, encode(h.ipui, 'hex') AS ipui, h.registered_at, h.last_seen_at,
				h.internal_number, h.disclosed_to_admins,
				coalesce((SELECT json_agg(json_build_object('line', l.line, 'voip_profile', a.label) ORDER BY l.line)
				          FROM handset_line l JOIN sip_account a ON a.id = l.sip_account_id WHERE l.handset_id = h.id), '[]') AS lines
			FROM handset h WHERE h.owner_user_id = $1 ORDER BY h.registered_at`},
		{"voip_profiles", `SELECT label, username, domain, created_at FROM sip_account WHERE owner_user_id = $1 ORDER BY lower(label)`},
		{"contacts", `SELECT c.last_name, c.first_name, c.created_at,
				coalesce((SELECT json_agg(json_build_object('number', n.number, 'kind', n.kind, 'default', n.is_default) ORDER BY n.position)
				          FROM contact_number n WHERE n.contact_id = c.id), '[]') AS numbers
			FROM contact c WHERE c.owner_user_id = $1 AND c.addressbook_id IS NULL ORDER BY lower(c.last_name), lower(c.first_name)`},
		{"address_books", `SELECT label, url, username, enabled, interval_min, last_ok_at, contact_count, created_at FROM addressbook
			WHERE owner_user_id = $1 ORDER BY lower(label)`},
		{"calls", `SELECT started_at, answered_at, ended_at, direction, remote_party, line, end_reason FROM call_log
			WHERE owner_user_id = $1 ORDER BY started_at DESC LIMIT ` + itoa(maxRows)},
		{"base_stations", `SELECT name, location, state, box_model, latitude, longitude, enrolled_at, disclosed_to_admins
			FROM cell WHERE enrolled_by = $1 ORDER BY lower(name)`},
		{"organizations", `SELECT o.name, m.display_name AS your_name, m.role, m.joined_at,
				(o.owner_user_id = m.user_id) AS owner
			FROM organization_member m JOIN organization o ON o.id = m.org_id WHERE m.user_id = $1 ORDER BY lower(o.name)`},
		{"audit_entries_by_you", `SELECT at, action, object_type FROM audit_log WHERE actor_user_id = $1 ORDER BY id DESC LIMIT ` + itoa(maxRows)},
	}
	out := map[string]any{"exported_at": time.Now().UTC(), "note": "Personal data held about this account. Secrets (password hash, handset keys, VoIP and address book passwords, two-factor secret) are not included."}
	for _, s := range sections {
		rows, err := pool.Query(ctx, "SELECT coalesce(json_agg(row_to_json(t)), '[]') FROM ("+s.query+") t", user)
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if rows.Next() {
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return nil, err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if s.name == "profile" { // one object, not a list
			var l []json.RawMessage
			if json.Unmarshal(raw, &l) == nil && len(l) == 1 {
				raw = l[0]
			}
		}
		out[s.name] = raw
	}
	return json.MarshalIndent(out, "", "  ")
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// EraseRecords finishes an erasure: the user's call history goes, the places of their base stations are blanked, the audit log forgets
// who they were, and the account is deleted (sessions, devices, contacts, two-factor and memberships go with it). The user must have no
// handsets and no VoIP lines left: remove them first.
func EraseRecords(ctx context.Context, pool *pgxpool.Pool, user uuid.UUID) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var role string
		if err := tx.QueryRow(ctx, "SELECT role FROM app_user WHERE id = $1 FOR UPDATE", user).Scan(&role); err != nil {
			return err
		}
		if role == "admin" {
			var others int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM app_user WHERE role = 'admin' AND id <> $1 AND NOT disabled", user).Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				return ErrLastAdmin
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM call_log WHERE owner_user_id = $1", user); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE cell SET location = '', latitude = NULL, longitude = NULL, box_model = '' WHERE enrolled_by = $1", user); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM app_user WHERE id = $1", user); err != nil { // fails while a handset or VoIP line remains
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT audit_forget_user($1)", user); err != nil {
			return err
		}
		return audit.RecordAs(ctx, tx, audit.System, audit.Entry{Action: "user.erased", ObjectType: "user", ObjectID: user.String()})
	})
}
