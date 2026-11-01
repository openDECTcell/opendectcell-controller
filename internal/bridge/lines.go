// SPDX-License-Identifier: AGPL-3.0-or-later

package bridge

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Receiver is a handset that has a SIP account as one of its lines: a call to
// the account rings it, on that line.
type Receiver struct {
	Handset uuid.UUID
	Line    int
}

// Lines says which SIP account a handset's line is, and which handsets an
// account rings (the handset_line table).
type Lines interface {
	// OutLine is the account a handset's call goes out on: the line it
	// chose, else its default line, else any line it has. ok is false when
	// it has none.
	OutLine(ctx context.Context, handset uuid.UUID, line int) (account uuid.UUID, ok bool, err error)
	// Receivers are the handsets an incoming call to the account rings.
	Receivers(ctx context.Context, account uuid.UUID) ([]Receiver, error)
}

// PGLines is Lines on the database.
type PGLines struct{ Pool *pgxpool.Pool }

// OutLine implements Lines.
func (p PGLines) OutLine(ctx context.Context, handset uuid.UUID, line int) (uuid.UUID, bool, error) {
	if handset == uuid.Nil {
		return uuid.Nil, false, nil
	}
	var acct uuid.UUID
	// the line asked for, then the default line, then the lowest
	err := p.Pool.QueryRow(ctx, `SELECT hl.sip_account_id FROM handset_line hl
		JOIN sip_account a ON a.id = hl.sip_account_id AND a.enabled
		JOIN handset h ON h.id = hl.handset_id
		WHERE hl.handset_id = $1 AND hl.enabled
		ORDER BY (hl.line = $2) DESC, (hl.line = h.default_line) DESC, hl.line
		LIMIT 1`, handset, line).Scan(&acct)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return acct, err == nil, err
}

// Receivers implements Lines.
func (p PGLines) Receivers(ctx context.Context, account uuid.UUID) ([]Receiver, error) {
	rows, err := p.Pool.Query(ctx, `SELECT DISTINCT ON (handset_id) handset_id, line FROM handset_line
		WHERE sip_account_id = $1 AND enabled ORDER BY handset_id, line`, account)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Receiver, error) {
		var x Receiver
		err := r.Scan(&x.Handset, &x.Line)
		return x, err
	})
}
