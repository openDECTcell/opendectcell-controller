// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sipaccount keeps SIP accounts in the database: the password
// sealed (docs/threat-model.md §4.2), the owner-or-admin rule on every
// access (AGENTS.md §5), an audit entry for every change. It hands the
// running SIP side its accounts with the passwords opened.
package sipaccount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

var (
	ErrNotFound = errors.New("sipaccount: not found")
	ErrInUse    = errors.New("sipaccount: the account is used by a handset line")
)

// ValidationError lists problems by field, for forms and the API.
type ValidationError map[string]string

func (v ValidationError) Error() string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + v[k]
	}
	return "invalid account: " + strings.Join(parts, "; ")
}

// Record is an account as shown to users. It never holds the password.
type Record struct {
	ID              uuid.UUID
	OwnerID         *uuid.UUID
	Label           string
	DisplayName     string
	Username        string
	Domain          string
	AuthUsername    string
	HasPassword     bool
	Registrar       string
	OutboundProxy   string
	Transport       string
	Register        bool
	RegisterExpiry  int
	Keepalive       string
	MediaEncryption string
	Codecs          []string
	DTMFMode        string
	CallerIDSource  string
	TLSVerify       bool
	Advanced        Advanced
	Enabled         bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Advanced holds the "should/later" parameters (docs/api/openapi.yaml,
// SipAdvanced). Only user_agent is used by the SIP side so far; the rest is
// stored for C4/C5 follow-ups.
type Advanced struct {
	UserAgent           string `json:"user_agent,omitempty"`
	SessionTimers       bool   `json:"session_timers,omitempty"`
	SessionExpires      int    `json:"session_expires,omitempty"`
	IdentityHeader      string `json:"identity_header,omitempty"`
	STUNServer          string `json:"stun_server,omitempty"`
	PublicAddress       string `json:"public_address,omitempty"`
	LocalPort           int    `json:"local_port,omitempty"`
	InterDigitTimeoutMs int    `json:"inter_digit_timeout_ms,omitempty"`
	LinePrefix          string `json:"line_prefix,omitempty"`
	PRACK               bool   `json:"prack,omitempty"`
}

// Warnings are problems that are allowed but unwise.
func (r *Record) Warnings() []string { return toSIP(r, "").Warnings() }

// Input is a create or update. Nil fields are left unchanged on update;
// on create they take the schema defaults. Password "" clears it; nil keeps.
type Input struct {
	OwnerID         **uuid.UUID // admin only; *OwnerID nil = shared
	Label           *string
	DisplayName     *string
	Username        *string
	Domain          *string
	AuthUsername    *string
	Password        *string
	Registrar       *string
	OutboundProxy   *string
	Transport       *string
	Register        *bool
	RegisterExpiry  *int
	Keepalive       *string
	MediaEncryption *string
	Codecs          *[]string
	DTMFMode        *string
	CallerIDSource  *string
	TLSVerify       *bool
	Advanced        *Advanced
	Enabled         *bool
}

// Store holds SIP accounts.
type Store struct {
	Pool    *pgxpool.Pool
	Keyring *secret.Keyring
	// OnChange, if set, runs after every committed change, e.g. to
	// reconcile the running user agents.
	OnChange func()
}

func (s *Store) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

const cols = `id, owner_user_id, label, display_name, username, domain, coalesce(auth_username, ''), password_enc IS NOT NULL,
	coalesce(registrar, ''), coalesce(outbound_proxy, ''), transport, register, register_expiry, keepalive, media_encryption,
	codecs, dtmf_mode, caller_id_source, tls_verify, advanced, enabled, created_at, updated_at`

func scan(r pgx.Row) (*Record, error) {
	var a Record
	var adv []byte
	err := r.Scan(&a.ID, &a.OwnerID, &a.Label, &a.DisplayName, &a.Username, &a.Domain, &a.AuthUsername, &a.HasPassword,
		&a.Registrar, &a.OutboundProxy, &a.Transport, &a.Register, &a.RegisterExpiry, &a.Keepalive, &a.MediaEncryption,
		&a.Codecs, &a.DTMFMode, &a.CallerIDSource, &a.TLSVerify, &adv, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(adv, &a.Advanced); err != nil {
		return nil, err
	}
	return &a, nil
}

// List returns the accounts u may see: their own, or for admins all of them
// (owner filters: nil = all, "shared", or a user id).
func (s *Store) List(ctx context.Context, u *auth.User, owner string) ([]*Record, error) {
	if u == nil {
		return nil, auth.ErrForbidden
	}
	q := "SELECT " + cols + " FROM sip_account WHERE "
	var args []any
	switch {
	case !u.IsAdmin():
		q += "owner_user_id = $1"
		args = append(args, u.ID)
	case owner == "shared":
		q += "owner_user_id IS NULL"
	case owner != "":
		id, err := uuid.Parse(owner)
		if err != nil {
			return nil, ValidationError{"owner": "must be a user id or 'shared'"}
		}
		q += "owner_user_id = $1"
		args = append(args, id)
	default:
		q += "true"
	}
	rows, err := s.Pool.Query(ctx, q+" ORDER BY lower(label), id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one account; ErrNotFound if it doesn't exist or u may not
// see it.
func (s *Store) Get(ctx context.Context, u *auth.User, id uuid.UUID) (*Record, error) {
	r, err := scan(s.Pool.QueryRow(ctx, "SELECT "+cols+" FROM sip_account WHERE id = $1", id))
	if err != nil {
		return nil, err
	}
	if !auth.CanAccess(u, r.OwnerID) {
		return nil, ErrNotFound
	}
	return r, nil
}

func loc(id uuid.UUID) secret.Location {
	return secret.Location{Table: "sip_account", Column: "password_enc", RowID: id.String()}
}

func defaults() *Record {
	return &Record{Transport: "tls", Register: true, RegisterExpiry: 600, Keepalive: "auto", MediaEncryption: "optional",
		Codecs: []string{"G722", "PCMA", "PCMU"}, DTMFMode: "rfc4733", CallerIDSource: "auto", TLSVerify: true, Enabled: true}
}

// apply merges in into r and reports the fields it changed.
func apply(r *Record, in Input) []string {
	var changed []string
	set := func(name string, dst *string, v *string) {
		if v != nil && *v != *dst {
			*dst = strings.TrimSpace(*v)
			changed = append(changed, name)
		}
	}
	set("label", &r.Label, in.Label)
	set("display_name", &r.DisplayName, in.DisplayName)
	set("username", &r.Username, in.Username)
	set("domain", &r.Domain, in.Domain)
	set("auth_username", &r.AuthUsername, in.AuthUsername)
	set("registrar", &r.Registrar, in.Registrar)
	set("outbound_proxy", &r.OutboundProxy, in.OutboundProxy)
	set("transport", &r.Transport, in.Transport)
	set("keepalive", &r.Keepalive, in.Keepalive)
	set("media_encryption", &r.MediaEncryption, in.MediaEncryption)
	set("dtmf_mode", &r.DTMFMode, in.DTMFMode)
	set("caller_id_source", &r.CallerIDSource, in.CallerIDSource)
	setB := func(name string, dst *bool, v *bool) {
		if v != nil && *v != *dst {
			*dst = *v
			changed = append(changed, name)
		}
	}
	setB("register", &r.Register, in.Register)
	setB("tls_verify", &r.TLSVerify, in.TLSVerify)
	setB("enabled", &r.Enabled, in.Enabled)
	if in.RegisterExpiry != nil && *in.RegisterExpiry != r.RegisterExpiry {
		r.RegisterExpiry = *in.RegisterExpiry
		changed = append(changed, "register_expiry")
	}
	if in.Codecs != nil && strings.Join(*in.Codecs, ",") != strings.Join(r.Codecs, ",") {
		r.Codecs = append([]string(nil), *in.Codecs...)
		changed = append(changed, "codecs")
	}
	if in.Advanced != nil && *in.Advanced != r.Advanced {
		r.Advanced = *in.Advanced
		changed = append(changed, "advanced")
	}
	if in.OwnerID != nil {
		nw, old := *in.OwnerID, r.OwnerID
		if (nw == nil) != (old == nil) || (nw != nil && *nw != *old) {
			r.OwnerID = nw
			changed = append(changed, "owner")
		}
	}
	return changed
}

func validate(r *Record) error {
	v := ValidationError{}
	if r.Label == "" || len(r.Label) > 64 {
		v["label"] = "1 to 64 characters"
	}
	for k, msg := range map[string]bool{"keepalive": oneOf(r.Keepalive, "auto", "none", "crlf", "options"),
		"media_encryption": oneOf(r.MediaEncryption, "off", "optional", "mandatory"),
		"dtmf_mode":        oneOf(r.DTMFMode, "rfc4733", "info", "inband"),
		"caller_id_source": oneOf(r.CallerIDSource, "auto", "from", "pai", "rpid")} {
		if !msg {
			v[k] = "not a valid choice"
		}
	}
	if r.Advanced.LinePrefix != "" && strings.Trim(r.Advanced.LinePrefix, "*#0123456789") != "" {
		v["advanced.line_prefix"] = "digits, * and # only"
	}
	if len(r.Advanced.UserAgent) > 128 {
		v["advanced.user_agent"] = "at most 128 characters"
	}
	if err := toSIP(r, "").Validate(); err != nil {
		v["account"] = err.Error()
	}
	if len(v) > 0 {
		return v
	}
	return nil
}

func oneOf(s string, opts ...string) bool {
	for _, o := range opts {
		if s == o {
			return true
		}
	}
	return false
}

// Create adds an account owned by u (admins may choose any owner or
// shared).
func (s *Store) Create(ctx context.Context, u *auth.User, in Input) (*Record, error) {
	if u == nil {
		return nil, auth.ErrForbidden
	}
	r := defaults()
	r.OwnerID = &u.ID
	if in.OwnerID != nil && !u.IsAdmin() {
		return nil, auth.ErrForbidden
	}
	apply(r, in)
	if err := validate(r); err != nil {
		return nil, err
	}
	if err := s.checkOwner(ctx, r.OwnerID); err != nil {
		return nil, err
	}
	adv, _ := json.Marshal(r.Advanced)
	var out *Record
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `INSERT INTO sip_account (owner_user_id, label, display_name, username, domain, auth_username,
			registrar, outbound_proxy, transport, register, register_expiry, keepalive, media_encryption, codecs, dtmf_mode,
			caller_id_source, tls_verify, advanced, enabled)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), nullif($8, ''), $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
			RETURNING id`,
			r.OwnerID, r.Label, r.DisplayName, r.Username, r.Domain, r.AuthUsername, r.Registrar, r.OutboundProxy, r.Transport,
			r.Register, r.RegisterExpiry, r.Keepalive, r.MediaEncryption, r.Codecs, r.DTMFMode, r.CallerIDSource, r.TLSVerify, adv, r.Enabled).Scan(&id)
		if err != nil {
			return err
		}
		if in.Password != nil && *in.Password != "" {
			if err := s.setPassword(ctx, tx, id, *in.Password); err != nil {
				return err
			}
		}
		if out, err = scan(tx.QueryRow(ctx, "SELECT "+cols+" FROM sip_account WHERE id = $1", id)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "sip_account.create", ObjectType: "sip_account", ObjectID: id.String(),
			Details: map[string]any{"label": r.Label, "aor": r.Username + "@" + r.Domain, "owner": ownerStr(r.OwnerID)}})
	})
	if err != nil {
		return nil, err
	}
	s.changed()
	return out, nil
}

func ownerStr(o *uuid.UUID) string {
	if o == nil {
		return "shared"
	}
	return o.String()
}

func (s *Store) checkOwner(ctx context.Context, owner *uuid.UUID) error {
	if owner == nil {
		return nil
	}
	var ok bool
	if err := s.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM app_user WHERE id = $1)", *owner).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ValidationError{"owner": "no such user"}
	}
	return nil
}

func (s *Store) setPassword(ctx context.Context, tx pgx.Tx, id uuid.UUID, pw string) error {
	if len(pw) > 256 {
		return ValidationError{"password": "at most 256 characters"} //nolint:gosec // G101: a field name, not a credential
	}
	var enc []byte
	if pw != "" {
		var err error
		if enc, err = s.Keyring.Seal(loc(id), []byte(pw)); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, "UPDATE sip_account SET password_enc = $2 WHERE id = $1", id, enc)
	return err
}

// Update changes an account u may access. Changing the owner is admin-only.
func (s *Store) Update(ctx context.Context, u *auth.User, id uuid.UUID, in Input) (*Record, error) {
	if in.OwnerID != nil && !u.IsAdmin() {
		return nil, auth.ErrForbidden
	}
	var out *Record
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		r, err := scan(tx.QueryRow(ctx, "SELECT "+cols+" FROM sip_account WHERE id = $1 FOR UPDATE", id))
		if err != nil {
			return err
		}
		if !auth.CanAccess(u, r.OwnerID) {
			return ErrNotFound
		}
		changed := apply(r, in)
		if in.Password != nil {
			changed = append(changed, "password")
		}
		if len(changed) == 0 {
			out = r
			return nil
		}
		if err := validate(r); err != nil {
			return err
		}
		if err := s.checkOwner(ctx, r.OwnerID); err != nil {
			return err
		}
		adv, _ := json.Marshal(r.Advanced)
		if _, err := tx.Exec(ctx, `UPDATE sip_account SET owner_user_id = $2, label = $3, display_name = $4, username = $5, domain = $6,
			auth_username = nullif($7, ''), registrar = nullif($8, ''), outbound_proxy = nullif($9, ''), transport = $10, register = $11,
			register_expiry = $12, keepalive = $13, media_encryption = $14, codecs = $15, dtmf_mode = $16, caller_id_source = $17,
			tls_verify = $18, advanced = $19, enabled = $20, updated_at = now() WHERE id = $1`,
			id, r.OwnerID, r.Label, r.DisplayName, r.Username, r.Domain, r.AuthUsername, r.Registrar, r.OutboundProxy, r.Transport,
			r.Register, r.RegisterExpiry, r.Keepalive, r.MediaEncryption, r.Codecs, r.DTMFMode, r.CallerIDSource, r.TLSVerify, adv, r.Enabled); err != nil {
			return err
		}
		if in.Password != nil {
			if err := s.setPassword(ctx, tx, id, *in.Password); err != nil {
				return err
			}
		}
		// The owner rule for lines (schema.md §3): if the owner changed,
		// lines on handsets the new owner doesn't own lose this account.
		if containsStr(changed, "owner") {
			if _, err := tx.Exec(ctx, `DELETE FROM handset_line hl USING handset h
				WHERE hl.handset_id = h.id AND hl.sip_account_id = $1
				  AND $2::uuid IS NOT NULL AND h.owner_user_id IS DISTINCT FROM $2`, id, r.OwnerID); err != nil {
				return err
			}
		}
		if out, err = scan(tx.QueryRow(ctx, "SELECT "+cols+" FROM sip_account WHERE id = $1", id)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "sip_account.update", ObjectType: "sip_account", ObjectID: id.String(),
			Details: map[string]any{"fields": changed}})
	})
	if err != nil {
		return nil, err
	}
	s.changed()
	return out, nil
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Delete removes an account u may access, unless a handset line uses it.
func (s *Store) Delete(ctx context.Context, u *auth.User, id uuid.UUID) error {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var owner *uuid.UUID
		var label string
		err := tx.QueryRow(ctx, "SELECT owner_user_id, label FROM sip_account WHERE id = $1 FOR UPDATE", id).Scan(&owner, &label)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !auth.CanAccess(u, owner) {
			return ErrNotFound
		}
		var used bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM handset_line WHERE sip_account_id = $1)", id).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrInUse
		}
		if _, err := tx.Exec(ctx, "DELETE FROM sip_account WHERE id = $1", id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "sip_account.delete", ObjectType: "sip_account", ObjectID: id.String(),
			Details: map[string]any{"label": label}})
	})
	if err != nil {
		return err
	}
	s.changed()
	return nil
}

func toSIP(r *Record, password string) sip.Account {
	return sip.Account{ID: r.ID, Label: r.Label, DisplayName: r.DisplayName, Username: r.Username, Domain: r.Domain,
		AuthUsername: r.AuthUsername, Password: password, Registrar: r.Registrar, OutboundProxy: r.OutboundProxy,
		Transport: resolve.Transport(r.Transport), Register: r.Register, RegisterExpiry: r.RegisterExpiry, Keepalive: r.Keepalive,
		Encryption: sdp.Encryption(r.MediaEncryption), Codecs: r.Codecs, DTMFMode: r.DTMFMode, CallerIDSource: r.CallerIDSource,
		TLSVerify: r.TLSVerify, Enabled: r.Enabled, UserAgent: r.Advanced.UserAgent}
}

// RuntimeAccounts returns every account for the SIP side, passwords
// opened. An account whose password cannot be opened is skipped and
// reported, never started without it.
func (s *Store) RuntimeAccounts(ctx context.Context) ([]sip.Account, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+cols+", password_enc FROM sip_account ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sip.Account
	var errs []error
	for rows.Next() {
		var enc []byte
		var a Record
		var adv []byte
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.Label, &a.DisplayName, &a.Username, &a.Domain, &a.AuthUsername, &a.HasPassword,
			&a.Registrar, &a.OutboundProxy, &a.Transport, &a.Register, &a.RegisterExpiry, &a.Keepalive, &a.MediaEncryption,
			&a.Codecs, &a.DTMFMode, &a.CallerIDSource, &a.TLSVerify, &adv, &a.Enabled, &a.CreatedAt, &a.UpdatedAt, &enc); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(adv, &a.Advanced)
		pw := ""
		if enc != nil {
			p, err := s.Keyring.Open(loc(a.ID), enc)
			if err != nil {
				errs = append(errs, fmt.Errorf("account %s: password: %w", a.ID, err))
				continue
			}
			pw = string(p)
		}
		out = append(out, toSIP(&a, pw))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, errors.Join(errs...)
}
