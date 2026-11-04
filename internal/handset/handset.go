// SPDX-License-Identifier: AGPL-3.0-or-later

// Package handset reads and edits what the web UI shows of a handset: its
// name, model, lines and ring policy, and where it was last seen. The DECT
// record itself (identity, keys) belongs to the DECT core; nothing here
// touches it.
package handset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

// ErrNotFound means no such handset, or one the user may not see.
var ErrNotFound = errors.New("handset: not found")

// ErrInvalid is returned for input that is out of range; its message is shown.
var ErrInvalid = errors.New("handset: invalid input")

// Line is one of a handset's two lines.
type Line struct {
	Line      int
	AccountID uuid.UUID
	Label     string // the account's label
	Address   string // user@domain
	Enabled   bool
}

// Handset is a handset as the UI lists it.
type Handset struct {
	ID           uuid.UUID
	OwnerID      uuid.UUID
	Owner        string
	Name         string
	Model        string
	Profile      string // the id of its device profile; "" is the standard behaviour
	RegisteredAt time.Time
	LastSeenAt   *time.Time
	LastSeenCell string
	DefaultLine  *int
	DualRing     string // call_waiting or busy
	Lines        []Line
	// Disclosed: the owner lets administrators see this handset (under the owner in Administration > Users).
	Disclosed     bool
	IPUI          []byte         // the identity it registered with (an IPEI for a type N)
	TPUI          int            // its temporary identity
	Capabilities  map[string]any // what it said of itself (TERMINAL-CAPABILITY), as the DECT core stored it
	SignalDBm     *int           // the last field strength measured on a link to it
	SignalAt      *time.Time
	CellsHoldingN int // cells that hold its record (a slot)
	// Pooled: every cell holds it, host-only, under a TPUI of its own
	// (docs/roaming.md); otherwise it sits in chip slots of its home cells.
	Pooled bool
	// InternalNumber is what another handset of the account dials after INT to reach this one
	// (0: it has none). Presence is worked out when the handset is read (PresenceAt).
	InternalNumber int
	DetachedAt     *time.Time
	PageFailedAt   *time.Time
	Presence       Presence
}

// WidebandClaim says whether the handset announced wideband speech (G.722) in
// its terminal capability. It is false until the handset has reported, so a
// page shows it only where Capabilities is set. Records the DECT core stored
// before it wrote the flag are read from the standard text. It is what the
// handset claims; a call may still use narrowband.
func (h Handset) WidebandClaim() bool {
	if v, ok := h.Capabilities["wideband"].(bool); ok {
		return v
	}
	std, _ := h.Capabilities["standard"].(string)
	return strings.HasPrefix(std, "NG-DECT")
}

// IPEI is the handset's IPEI as printed on its label, "EEEEE PPPPPPP C" (EN
// 300 175-6 annex C), with the manufacturer code (EMC) alone; ok is false for
// an identity that is not an IPEI (another type of IPUI).
func (h Handset) IPEI() (text string, emc int, ok bool) {
	p := h.IPUI
	// IPUI type (octet 3: 1 000 0000), 40 bits (octet 4), PUT N (0000), then
	// the 36-bit IPEI: EMC 16 bits and PSN 20 bits.
	if len(p) != 7 || p[0] != 0x80 || p[1] != 0x80|40 || p[2]>>4 != 0 {
		return "", 0, false
	}
	var v uint64
	for _, b := range p[2:] {
		v = v<<8 | uint64(b)
	}
	emc, psn := int(v>>20&0xffff), int(v&0xfffff)
	digits := fmt.Sprintf("%05d%07d", emc, psn)
	sum := 0
	for i, d := range digits {
		sum += int(d-'0') * (i + 1)
	}
	sum %= 11
	check := byte('0' + sum)
	if sum == 10 {
		check = '*'
	}
	return fmt.Sprintf("%05d %07d %c", emc, psn, check), emc, true
}

// Move is a handset heard on another cell than the time before.
type Move struct {
	At       time.Time
	From, To string // cell names; "" for a cell that is gone
}

// Display is the name, or a fallback for a handset nobody named.
func (h Handset) Display() string {
	if h.Name != "" {
		return h.Name
	}
	if h.Model != "" {
		return h.Model
	}
	return "Handset"
}

// Store reads and edits handsets.
type Store struct {
	Pool *pgxpool.Pool
	// Interval is how often handsets are asked to register again (the roaming setting), for their
	// presence; nil or 0: not asked, so silence says nothing.
	Interval func(context.Context) time.Duration
}

// presence sets the presence of the handsets read, as of now.
func (s *Store) presence(ctx context.Context, hs ...*Handset) {
	var iv time.Duration
	if s.Interval != nil {
		iv = s.Interval(ctx)
	}
	now := time.Now()
	for _, h := range hs {
		h.Presence = PresenceAt(now, h.LastSeenAt, h.DetachedAt, h.PageFailedAt, iv)
	}
}

const cols = `h.id, h.owner_user_id, coalesce(nullif(u.display_name, ''), u.username), h.display_name, coalesce(h.model, ''), h.registered_at,
	h.last_seen_at, coalesce(c.name, ''), h.default_line, h.dual_ring_policy,
	(SELECT count(*) FROM cell_slot s WHERE s.handset_id = h.id), h.pooled, coalesce(h.profile, ''),
	h.ipui, h.tpui, h.capabilities, h.signal_dbm, h.signal_at, h.internal_number, h.detached_at, h.page_failed_at, h.disclosed_to_admins`

const from = ` FROM handset h JOIN app_user u ON u.id = h.owner_user_id LEFT JOIN cell c ON c.id = h.last_seen_cell_id`

func scan(r pgx.Row) (*Handset, error) {
	var h Handset
	var caps []byte
	var dbm, num *int16
	err := r.Scan(&h.ID, &h.OwnerID, &h.Owner, &h.Name, &h.Model, &h.RegisteredAt, &h.LastSeenAt, &h.LastSeenCell, &h.DefaultLine, &h.DualRing, &h.CellsHoldingN, &h.Pooled, &h.Profile,
		&h.IPUI, &h.TPUI, &caps, &dbm, &h.SignalAt, &num, &h.DetachedAt, &h.PageFailedAt, &h.Disclosed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(caps) > 0 {
		_ = json.Unmarshal(caps, &h.Capabilities) // a value we did not write is shown as nothing
	}
	if dbm != nil {
		v := int(*dbm)
		h.SignalDBm = &v
	}
	if num != nil {
		h.InternalNumber = int(*num)
	}
	return &h, nil
}

// List returns the handsets u owns. A handset is private to its owner: an
// administrator does not see other users' handsets. Lines are filled in.
func (s *Store) List(ctx context.Context, u *auth.User) ([]*Handset, error) {
	return s.list(ctx, " WHERE h.owner_user_id = $1 ORDER BY h.registered_at", u.ID, true)
}

// Disclosed returns the handsets that user disclosed to the administrators; only an administrator
// may ask. Their lines stay private: they hold the owner's VoIP accounts.
func (s *Store) Disclosed(ctx context.Context, admin *auth.User, user uuid.UUID) ([]*Handset, error) {
	if auth.RequireAdmin(admin) != nil {
		return nil, ErrNotFound
	}
	return s.list(ctx, " WHERE h.owner_user_id = $1 AND h.disclosed_to_admins ORDER BY h.registered_at", user, false)
}

func (s *Store) list(ctx context.Context, where string, owner uuid.UUID, withLines bool) ([]*Handset, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+cols+from+where, owner)
	if err != nil {
		return nil, err
	}
	var out []*Handset
	byID := map[uuid.UUID]*Handset{}
	for rows.Next() {
		h, err := scan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, h)
		byID[h.ID] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !withLines {
		s.presence(ctx, out...)
		return out, nil
	}
	ids := make([]uuid.UUID, 0, len(out))
	for _, h := range out {
		ids = append(ids, h.ID)
	}
	lr, err := s.Pool.Query(ctx, `SELECT hl.handset_id, hl.line, a.id, a.label, a.username || '@' || a.domain, hl.enabled
		FROM handset_line hl JOIN sip_account a ON a.id = hl.sip_account_id WHERE hl.handset_id = ANY($1) ORDER BY hl.line`, ids)
	if err != nil {
		return nil, err
	}
	defer lr.Close()
	for lr.Next() {
		var hid uuid.UUID
		var l Line
		if err := lr.Scan(&hid, &l.Line, &l.AccountID, &l.Label, &l.Address, &l.Enabled); err != nil {
			return nil, err
		}
		if h := byID[hid]; h != nil {
			h.Lines = append(h.Lines, l)
		}
	}
	s.presence(ctx, out...)
	return out, lr.Err()
}

// Get returns one handset if u owns it; for anyone else it is not found.
func (s *Store) Get(ctx context.Context, u *auth.User, id uuid.UUID) (*Handset, error) {
	h, err := scan(s.Pool.QueryRow(ctx, "SELECT "+cols+from+" WHERE h.id = $1", id))
	if err != nil {
		return nil, err
	}
	if !auth.IsOwner(u, &h.OwnerID) {
		return nil, ErrNotFound
	}
	all, err := s.List(ctx, u)
	if err != nil {
		return nil, err
	}
	for _, x := range all {
		if x.ID == id {
			return x, nil
		}
	}
	s.presence(ctx, h)
	return h, nil
}

// Moves are the last n times the handset was heard on another cell, newest first.
// The caller has checked the access to the handset (Get).
func (s *Store) Moves(ctx context.Context, id uuid.UUID, n int) ([]Move, error) {
	rows, err := s.Pool.Query(ctx, `SELECT m.at, coalesce(f.name, ''), coalesce(t.name, '')
		FROM handset_move m LEFT JOIN cell f ON f.id = m.from_cell_id LEFT JOIN cell t ON t.id = m.to_cell_id
		WHERE m.handset_id = $1 ORDER BY m.at DESC, m.id DESC LIMIT $2`, id, n)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Move, error) {
		var m Move
		err := r.Scan(&m.At, &m.From, &m.To)
		return m, err
	})
}

// Update is the edit of a handset's own fields.
type Update struct {
	Name        string
	DefaultLine *int // nil: none
	DualRing    string
	// Profile, if set, is the handset's device profile id ("" for none) and
	// Model the name the pages show for it; nil keeps both as they are.
	Profile *string
	Model   string
	// Disclosed lets administrators see the handset under its owner.
	Disclosed bool
}

// Update changes the name, default line and ring policy. A default line
// must be one of the handset's lines.
func (s *Store) Update(ctx context.Context, u *auth.User, id uuid.UUID, up Update) error {
	if len(up.Name) > 64 {
		return ErrInvalid
	}
	if up.DualRing != "call_waiting" && up.DualRing != "busy" {
		return ErrInvalid
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var owner uuid.UUID
		if err := tx.QueryRow(ctx, "SELECT owner_user_id FROM handset WHERE id = $1 FOR UPDATE", id).Scan(&owner); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if !auth.IsOwner(u, &owner) {
			return ErrNotFound
		}
		if up.DefaultLine != nil {
			var has bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM handset_line WHERE handset_id = $1 AND line = $2)", id, *up.DefaultLine).Scan(&has); err != nil {
				return err
			}
			if !has {
				return ErrInvalid
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE handset SET display_name = $2, default_line = $3, dual_ring_policy = $4, disclosed_to_admins = $5 WHERE id = $1", id, up.Name, up.DefaultLine, up.DualRing, up.Disclosed); err != nil {
			return err
		}
		if up.Profile != nil {
			if _, err := tx.Exec(ctx, "UPDATE handset SET profile = nullif($2, ''), model = nullif($3, '') WHERE id = $1", id, *up.Profile, up.Model); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "handset.update", ObjectType: "handset", ObjectID: id.String(),
			Details: map[string]any{"dual_ring": up.DualRing, "default_line": up.DefaultLine, "disclosed_to_admins": up.Disclosed}})
	})
}

// Delete removes the handset: its row, lines and history (the call log keeps
// its entries without it). The cells' records are the DECT core's to drop; this
// leaves their slots empty in the store. Only its owner may.
func (s *Store) Delete(ctx context.Context, u *auth.User, id uuid.UUID) error {
	return s.remove(ctx, id, "handset.delete", func(owner uuid.UUID) bool { return auth.IsOwner(u, &owner) })
}

func (s *Store) remove(ctx context.Context, id uuid.UUID, action string, may func(owner uuid.UUID) bool) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var owner uuid.UUID
		if err := tx.QueryRow(ctx, "SELECT owner_user_id FROM handset WHERE id = $1 FOR UPDATE", id).Scan(&owner); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if !may(owner) {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, "UPDATE cell_slot SET handset_id = NULL, state = 'empty', written_at = now() WHERE handset_id = $1", id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM handset WHERE id = $1", id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: action, ObjectType: "handset", ObjectID: id.String(),
			Details: map[string]any{"owner_id": owner.String()}})
	})
}

// ErrBadIPEI is returned for text that is not an IPEI as printed on a label.
var ErrBadIPEI = errors.New("handset: not an IPEI (EEEEE PPPPPPP C: five digits, seven digits, a check character)")

// IPUIFromIPEI is the identity a handset with this IPEI registers with (the inverse of Handset.IPEI). The check
// character is verified if given.
func IPUIFromIPEI(text string) ([]byte, error) {
	var digits []byte
	var check byte
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == ' ' || c == '-':
		case c >= '0' && c <= '9' && len(digits) < 12:
			digits = append(digits, c)
		case (c >= '0' && c <= '9' || c == '*') && len(digits) == 12 && check == 0:
			check = c
		default:
			return nil, ErrBadIPEI
		}
	}
	if len(digits) != 12 {
		return nil, ErrBadIPEI
	}
	var emc, psn uint64
	for _, d := range digits[:5] {
		emc = emc*10 + uint64(d-'0')
	}
	for _, d := range digits[5:] {
		psn = psn*10 + uint64(d-'0')
	}
	if emc > 0xffff || psn > 0xfffff {
		return nil, ErrBadIPEI
	}
	if check != 0 {
		sum := 0
		for i, d := range digits {
			sum += int(d-'0') * (i + 1)
		}
		want := byte('0' + sum%11)
		if sum%11 == 10 {
			want = '*'
		}
		if check != want {
			return nil, ErrBadIPEI
		}
	}
	v := emc<<20 | psn
	return []byte{0x80, 0x80 | 40, byte(v >> 32 & 0x0f), byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}, nil
}

// Claim is what an administrator is shown of a handset they are asked to release: whose it is and since when, nothing else.
type Claim struct {
	ID           uuid.UUID
	Owner        string
	RegisteredAt time.Time
}

// FindClaim looks up the handset registered under an IPEI, for an administrator who is asked to release it (a handset
// registered under someone else's name stops its real owner from registering it). ErrNotFound if none is.
func (s *Store) FindClaim(ctx context.Context, admin *auth.User, ipei string) (*Claim, error) {
	if auth.RequireAdmin(admin) != nil {
		return nil, ErrNotFound
	}
	ipui, err := IPUIFromIPEI(ipei)
	if err != nil {
		return nil, err
	}
	var c Claim
	err = s.Pool.QueryRow(ctx, `SELECT h.id, coalesce(nullif(u.display_name, ''), u.username), h.registered_at
		FROM handset h JOIN app_user u ON u.id = h.owner_user_id WHERE h.ipui = $1`, ipui).Scan(&c.ID, &c.Owner, &c.RegisteredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// Release removes a handset on an administrator's word, whoever owns it: its row, lines and history go, and the slots
// it held are freed. The audit log records who did it and whose handset it was. The caller tells the cells to drop the record.
func (s *Store) Release(ctx context.Context, admin *auth.User, id uuid.UUID) error {
	if auth.RequireAdmin(admin) != nil {
		return ErrNotFound
	}
	return s.remove(ctx, id, "handset.released_by_admin", func(uuid.UUID) bool { return true })
}

// Account is a phone line account a handset line may use.
type Account struct {
	ID    uuid.UUID
	Label string
}

// Accounts lists the accounts the handset's owner may put on a line: their
// own and the shared ones (owner NULL).
func (s *Store) Accounts(ctx context.Context, owner uuid.UUID) ([]Account, error) {
	rows, err := s.Pool.Query(ctx, "SELECT id, label FROM sip_account WHERE owner_user_id = $1 OR owner_user_id IS NULL ORDER BY lower(label)", owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Label); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetLine puts account on line 1 or 2 of a handset, or clears the line when
// account is uuid.Nil. The account must be one the owner may use. Clearing
// the default line clears the default.
func (s *Store) SetLine(ctx context.Context, u *auth.User, id uuid.UUID, line int, account uuid.UUID) error {
	if line != 1 && line != 2 {
		return ErrInvalid
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var owner uuid.UUID
		if err := tx.QueryRow(ctx, "SELECT owner_user_id FROM handset WHERE id = $1 FOR UPDATE", id).Scan(&owner); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if owner != u.ID {
			return ErrNotFound
		}
		if account == uuid.Nil {
			if _, err := tx.Exec(ctx, "DELETE FROM handset_line WHERE handset_id = $1 AND line = $2", id, line); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE handset SET default_line = NULL WHERE id = $1 AND default_line = $2", id, line); err != nil {
				return err
			}
		} else {
			var ok bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM sip_account WHERE id = $1 AND (owner_user_id = $2 OR owner_user_id IS NULL))", account, owner).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return ErrInvalid
			}
			if _, err := tx.Exec(ctx, `INSERT INTO handset_line (handset_id, line, sip_account_id) VALUES ($1, $2, $3)
				ON CONFLICT (handset_id, line) DO UPDATE SET sip_account_id = EXCLUDED.sip_account_id`, id, line, account); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "handset.line", ObjectType: "handset", ObjectID: id.String(),
			Details: map[string]any{"line": line, "account": account.String()}})
	})
}

// PresenceCounts is how many handsets the given owners have, and how many of them are online now.
func (s *Store) PresenceCounts(ctx context.Context, owners []uuid.UUID) (total, online int, err error) {
	rows, err := s.Pool.Query(ctx, "SELECT last_seen_at, detached_at, page_failed_at FROM handset WHERE owner_user_id = ANY($1)", owners)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var iv time.Duration
	if s.Interval != nil {
		iv = s.Interval(ctx)
	}
	now := time.Now()
	for rows.Next() {
		var seen, det, failed *time.Time
		if err := rows.Scan(&seen, &det, &failed); err != nil {
			return 0, 0, err
		}
		total++
		if PresenceAt(now, seen, det, failed, iv) == Online {
			online++
		}
	}
	return total, online, rows.Err()
}
