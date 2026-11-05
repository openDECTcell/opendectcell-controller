// SPDX-License-Identifier: AGPL-3.0-or-later

// Package calllog records calls in call_log: who, when, how it ended, the
// codecs on both legs, and the media quality (docs/media-engine.md §7). The
// DECT core (C3) and the bridge write it as calls progress; the admin UI and
// the call lists read it.
package calllog

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/logctl"
	"github.com/openDECTcell/opendectcell-controller/internal/media"
)

// ErrNotFound means no such call.
var ErrNotFound = errors.New("calllog: not found")

// Call is one call_log row.
type Call struct {
	ID           int64
	HandsetID    *uuid.UUID
	OwnerID      *uuid.UUID
	Line         *int
	SIPAccountID *uuid.UUID
	Direction    string // in, out
	RemoteParty  string
	StartedAt    time.Time
	AnsweredAt   *time.Time
	EndedAt      *time.Time
	EndReason    *string
	CellIDs      []uuid.UUID
	SIPCodec     *string
	CellCodec    *string
	SIPTLS       *bool
	SRTP         *bool
	Quality      *media.Quality // nil until the call has ended with media
	// MissedReadAt is when an unanswered incoming call was marked as seen (by a handset's call list or on the web).
	MissedReadAt *time.Time
}

// Missed is an incoming call nobody answered.
func (c Call) Missed() bool { return c.Direction == "in" && c.AnsweredAt == nil && c.EndedAt != nil }

// Duration is the answered time, zero for an unanswered or ongoing call.
func (c Call) Duration() time.Duration {
	if c.AnsweredAt == nil || c.EndedAt == nil {
		return 0
	}
	return c.EndedAt.Sub(*c.AnsweredAt)
}

// Store reads and writes call_log.
type Store struct {
	Pool *pgxpool.Pool
	// Log, if set, gets one line per call event at the calls level
	// (logctl.LevelCalls), with the remote party masked.
	Log *slog.Logger
	// Hook, if set, is told after a call was recorded as started or as ended, with the call's id (the webhooks read the row). It runs on
	// the call's own path and must return at once.
	Hook func(ctx context.Context, event string, id int64)
}

// The events of Store.Hook.
const (
	EventStarted = "started"
	EventEnded   = "ended"
)

func (s *Store) hook(ctx context.Context, event string, id int64) {
	if s.Hook != nil {
		s.Hook(ctx, event, id)
	}
}

func (s *Store) note(ctx context.Context, msg string, args ...any) {
	if s.Log != nil {
		s.Log.Log(ctx, logctl.LevelCalls, msg, args...)
	}
}

// Start records a new call. The owner is the handset owner at this moment, a
// snapshot that survives the handset (docs/schema.md).
func (s *Store) Start(ctx context.Context, c Call) (int64, error) {
	if c.StartedAt.IsZero() {
		c.StartedAt = time.Now()
	}
	var id int64
	err := s.Pool.QueryRow(ctx, `INSERT INTO call_log (handset_id, owner_user_id, line, sip_account_id, direction, remote_party, started_at, cell_ids)
		VALUES ($1, coalesce($2, (SELECT owner_user_id FROM handset WHERE id = $1)), $3, $4, $5, $6, $7, coalesce($8::uuid[], '{}'))
		RETURNING id`,
		c.HandsetID, c.OwnerID, c.Line, c.SIPAccountID, c.Direction, c.RemoteParty, c.StartedAt, c.CellIDs).Scan(&id)
	if err == nil {
		s.note(ctx, "call started", "call", id, "direction", c.Direction, "remote", logctl.MaskNumber(c.RemoteParty), "line", c.Line, "cells", c.CellIDs)
		s.hook(ctx, EventStarted, id)
	}
	return id, err
}

// Answered records when the call was answered.
func (s *Store) Answered(ctx context.Context, id int64, at time.Time) error {
	err := s.exec(ctx, "UPDATE call_log SET answered_at = $2 WHERE id = $1 AND answered_at IS NULL", id, at)
	if err == nil {
		s.note(ctx, "call answered", "call", id)
	}
	return err
}

// End is how a call finished.
type End struct {
	At        time.Time
	Reason    string
	SIPCodec  string
	CellCodec string
	SIPTLS    *bool
	SRTP      *bool
	Quality   *media.Quality // nil if no media flowed
}

// Ended records the end of a call, its codecs and its media quality.
func (s *Store) Ended(ctx context.Context, id int64, e End) error {
	q := []byte("{}")
	if e.Quality != nil {
		var err error
		if q, err = json.Marshal(e.Quality); err != nil {
			return err
		}
	}
	err := s.exec(ctx, `UPDATE call_log SET ended_at = $2, end_reason = $3, sip_codec = nullif($4, ''), cell_codec = nullif($5, ''),
		sip_tls = $6, srtp = $7, quality = $8 WHERE id = $1`, id, e.At, e.Reason, e.SIPCodec, e.CellCodec, e.SIPTLS, e.SRTP, q)
	if err == nil {
		s.note(ctx, "call ended", "call", id, "reason", e.Reason, "sip_codec", e.SIPCodec, "cell_codec", e.CellCodec, "srtp", e.SRTP != nil && *e.SRTP)
		s.hook(ctx, EventEnded, id)
	}
	return err
}

func (s *Store) exec(ctx context.Context, sql string, args ...any) error {
	tag, err := s.Pool.Exec(ctx, sql, args...)
	if err == nil && tag.RowsAffected() == 0 {
		var exists bool
		if err := s.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM call_log WHERE id = $1)", args[0]).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return err
}

// Filter selects calls, newest first.
type Filter struct {
	Owner     *uuid.UUID // nil: everyone's (admins)
	Handset   *uuid.UUID
	Account   *uuid.UUID // the VoIP line
	Direction string     // "in", "out" or "" for both
	Missed    bool       // only incoming calls nobody answered
	Since     time.Time
	Before    int64 // id cursor; 0 for the newest
	Limit     int   // default 50, at most 500
}

const callCols = `id, handset_id, owner_user_id, line, sip_account_id, direction, remote_party, started_at,
	answered_at, ended_at, end_reason, cell_ids, sip_codec, cell_codec, sip_tls, srtp, quality, missed_read_at`

func scanCall(r pgx.Row) (Call, error) {
	var c Call
	var line *int16
	var q []byte
	err := r.Scan(&c.ID, &c.HandsetID, &c.OwnerID, &line, &c.SIPAccountID, &c.Direction, &c.RemoteParty, &c.StartedAt,
		&c.AnsweredAt, &c.EndedAt, &c.EndReason, &c.CellIDs, &c.SIPCodec, &c.CellCodec, &c.SIPTLS, &c.SRTP, &q, &c.MissedReadAt)
	if line != nil {
		l := int(*line)
		c.Line = &l
	}
	if err == nil && len(q) > 2 { // "{}" is no quality
		c.Quality = new(media.Quality)
		err = json.Unmarshal(q, c.Quality)
	}
	return c, err
}

// List returns calls matching f.
func (s *Store) List(ctx context.Context, f Filter) ([]Call, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+callCols+`
		FROM call_log
		WHERE ($1::uuid IS NULL OR owner_user_id = $1) AND ($2::uuid IS NULL OR handset_id = $2)
			AND started_at >= $3 AND ($4 = 0 OR id < $4)
			AND ($6::uuid IS NULL OR sip_account_id = $6) AND ($7 = '' OR direction = $7)
			AND (NOT $8 OR (direction = 'in' AND answered_at IS NULL AND ended_at IS NOT NULL))
		ORDER BY id DESC LIMIT $5`, f.Owner, f.Handset, f.Since, f.Before, f.Limit, f.Account, f.Direction, f.Missed)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Call, error) { return scanCall(r) })
}

// Get returns one call. The caller checks that the user may see it (its owner).
func (s *Store) Get(ctx context.Context, id int64) (Call, error) {
	c, err := scanCall(s.Pool.QueryRow(ctx, "SELECT "+callCols+" FROM call_log WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Call{}, ErrNotFound
	}
	return c, err
}

// UnreadMissed counts a user's missed calls nobody has marked as seen.
func (s *Store) UnreadMissed(ctx context.Context, owner uuid.UUID) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM call_log WHERE owner_user_id = $1 AND direction = 'in'
		AND answered_at IS NULL AND ended_at IS NOT NULL AND missed_read_at IS NULL`, owner).Scan(&n)
	return n, err
}

// MarkMissedRead marks all of a user's missed calls as seen, the way a handset's call list does one by one.
func (s *Store) MarkMissedRead(ctx context.Context, owner uuid.UUID) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE call_log SET missed_read_at = now() WHERE owner_user_id = $1 AND direction = 'in'
		AND answered_at IS NULL AND ended_at IS NOT NULL AND missed_read_at IS NULL`, owner)
	return tag.RowsAffected(), err
}

// ActiveWindow is how long a call without an end still counts as going on: a call whose end was never recorded (a
// crash) must not stay "right now" for ever.
const ActiveWindow = 6 * time.Hour

// Active returns the calls going on now (ringing or answered, not ended), newest first: one user's, or everybody's
// for a nil owner.
func (s *Store) Active(ctx context.Context, owner *uuid.UUID) ([]Call, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+callCols+` FROM call_log
		WHERE ended_at IS NULL AND started_at > $2 AND ($1::uuid IS NULL OR owner_user_id = $1)
		ORDER BY id DESC LIMIT 200`, owner, time.Now().Add(-ActiveWindow))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Call, error) { return scanCall(r) })
}
