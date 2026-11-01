// SPDX-License-Identifier: AGPL-3.0-or-later

// Package handset reads and edits what the web UI shows of a handset: its
// name, model, lines and ring policy, and where it was last seen. The DECT
// record itself (identity, keys) belongs to the DECT core; nothing here
// touches it.
package handset

import (
	"context"
	"errors"
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
	ID            uuid.UUID
	OwnerID       uuid.UUID
	Owner         string
	Name          string
	Model         string
	Profile       string // the id of its device profile; "" is the standard behaviour
	RegisteredAt  time.Time
	LastSeenAt    *time.Time
	LastSeenCell  string
	DefaultLine   *int
	DualRing      string // call_waiting or busy
	Lines         []Line
	CellsHoldingN int // cells that hold its record (a slot)
	// Pooled: every cell holds it, host-only, under a TPUI of its own
	// (docs/roaming.md); otherwise it sits in chip slots of its home cells.
	Pooled bool
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
}

const cols = `h.id, h.owner_user_id, coalesce(nullif(u.display_name, ''), u.username), h.display_name, coalesce(h.model, ''), h.registered_at,
	h.last_seen_at, coalesce(c.name, ''), h.default_line, h.dual_ring_policy,
	(SELECT count(*) FROM cell_slot s WHERE s.handset_id = h.id), h.pooled, coalesce(h.profile, '')`

const from = ` FROM handset h JOIN app_user u ON u.id = h.owner_user_id LEFT JOIN cell c ON c.id = h.last_seen_cell_id`

func scan(r pgx.Row) (*Handset, error) {
	var h Handset
	err := r.Scan(&h.ID, &h.OwnerID, &h.Owner, &h.Name, &h.Model, &h.RegisteredAt, &h.LastSeenAt, &h.LastSeenCell, &h.DefaultLine, &h.DualRing, &h.CellsHoldingN, &h.Pooled, &h.Profile)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &h, err
}

// List returns the handsets u may see: their own, or every handset for an
// admin. Lines are filled in.
func (s *Store) List(ctx context.Context, u *auth.User) ([]*Handset, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+cols+from+" WHERE $1 OR h.owner_user_id = $2 ORDER BY lower(u.username), h.registered_at", u.IsAdmin(), u.ID)
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
	lr, err := s.Pool.Query(ctx, `SELECT hl.handset_id, hl.line, a.id, a.label, a.username || '@' || a.domain, hl.enabled
		FROM handset_line hl JOIN sip_account a ON a.id = hl.sip_account_id ORDER BY hl.line`)
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
	return out, lr.Err()
}

// Get returns one handset if u may see it.
func (s *Store) Get(ctx context.Context, u *auth.User, id uuid.UUID) (*Handset, error) {
	h, err := scan(s.Pool.QueryRow(ctx, "SELECT "+cols+from+" WHERE h.id = $1", id))
	if err != nil {
		return nil, err
	}
	if err := auth.RequireAccess(u, &h.OwnerID); err != nil {
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
		if auth.RequireAccess(u, &owner) != nil {
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
		if _, err := tx.Exec(ctx, "UPDATE handset SET display_name = $2, default_line = $3, dual_ring_policy = $4 WHERE id = $1", id, up.Name, up.DefaultLine, up.DualRing); err != nil {
			return err
		}
		if up.Profile != nil {
			if _, err := tx.Exec(ctx, "UPDATE handset SET profile = nullif($2, ''), model = nullif($3, '') WHERE id = $1", id, *up.Profile, up.Model); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "handset.update", ObjectType: "handset", ObjectID: id.String(),
			Details: map[string]any{"dual_ring": up.DualRing, "default_line": up.DefaultLine}})
	})
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
		if auth.RequireAccess(u, &owner) != nil {
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
