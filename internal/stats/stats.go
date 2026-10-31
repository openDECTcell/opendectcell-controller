// SPDX-License-Identifier: AGPL-3.0-or-later

// Package stats answers the statistics pages from the call log and the
// handset, account and cell tables. Every query takes a Scope: the whole
// controller (admins) or one user's own equipment.
package stats

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Scope limits a query to one owner. The zero value is the whole controller.
type Scope struct {
	User *uuid.UUID
}

// Own is the scope of one user's equipment.
func Own(id uuid.UUID) Scope { return Scope{User: &id} }

// Store runs the queries.
type Store struct {
	Pool *pgxpool.Pool
	Now  func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Summary is the headline numbers.
type Summary struct {
	Handsets       int
	SIPAccounts    int
	Calls24h       int
	Calls7d        int
	Answered7d     int
	Failed7d       int   // calls that never reached a conversation, excluding unanswered incoming ones
	TalkSeconds7d  int64 // conversation time
	Encrypted7d    int   // calls with SRTP on the SIP side
	CallsTotal     int
	LongestSeconds int64 // the longest conversation of the last 7 days
}

// Summary returns the headline numbers for scope.
func (s *Store) Summary(ctx context.Context, sc Scope) (Summary, error) {
	var m Summary
	now := s.now()
	err := s.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM handset WHERE $1::uuid IS NULL OR owner_user_id = $1),
		(SELECT count(*) FROM sip_account WHERE $1::uuid IS NULL OR owner_user_id = $1)`, sc.User).
		Scan(&m.Handsets, &m.SIPAccounts)
	if err != nil {
		return m, err
	}
	err = s.Pool.QueryRow(ctx, `SELECT
		count(*),
		count(*) FILTER (WHERE started_at >= $2),
		count(*) FILTER (WHERE started_at >= $3),
		count(*) FILTER (WHERE started_at >= $3 AND answered_at IS NOT NULL),
		count(*) FILTER (WHERE started_at >= $3 AND answered_at IS NULL AND direction = 'out'),
		coalesce(sum(extract(epoch FROM ended_at - answered_at)) FILTER (WHERE started_at >= $3 AND answered_at IS NOT NULL AND ended_at IS NOT NULL), 0)::bigint,
		count(*) FILTER (WHERE started_at >= $3 AND srtp),
		coalesce(max(extract(epoch FROM ended_at - answered_at)) FILTER (WHERE started_at >= $3 AND answered_at IS NOT NULL AND ended_at IS NOT NULL), 0)::bigint
		FROM call_log WHERE $1::uuid IS NULL OR owner_user_id = $1`,
		sc.User, now.Add(-24*time.Hour), now.Add(-7*24*time.Hour)).
		Scan(&m.CallsTotal, &m.Calls24h, &m.Calls7d, &m.Answered7d, &m.Failed7d, &m.TalkSeconds7d, &m.Encrypted7d, &m.LongestSeconds)
	return m, err
}

// Day is one day of calls.
type Day struct {
	Day         time.Time
	Calls       int
	Answered    int
	TalkSeconds int64
}

// Days returns the last n days, oldest first, including days without calls.
func (s *Store) Days(ctx context.Context, sc Scope, n int) ([]Day, error) {
	now := s.now()
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	start := end.AddDate(0, 0, -n)
	rows, err := s.Pool.Query(ctx, `SELECT d::date,
		count(c.id), count(c.id) FILTER (WHERE c.answered_at IS NOT NULL),
		coalesce(sum(extract(epoch FROM c.ended_at - c.answered_at)) FILTER (WHERE c.answered_at IS NOT NULL AND c.ended_at IS NOT NULL), 0)::bigint
		FROM generate_series($2::timestamptz, $3::timestamptz - interval '1 day', interval '1 day') d
		LEFT JOIN call_log c ON c.started_at >= d AND c.started_at < d + interval '1 day' AND ($1::uuid IS NULL OR c.owner_user_id = $1)
		GROUP BY d ORDER BY d`, sc.User, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Day
	for rows.Next() {
		var d Day
		if err := rows.Scan(&d.Day, &d.Calls, &d.Answered, &d.TalkSeconds); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Reason counts calls by how they ended.
type Reason struct {
	Reason string
	Count  int
}

// EndReasons returns how the last days' calls ended, most common first.
func (s *Store) EndReasons(ctx context.Context, sc Scope, days int) ([]Reason, error) {
	rows, err := s.Pool.Query(ctx, `SELECT coalesce(nullif(end_reason, ''), CASE WHEN answered_at IS NULL THEN 'not answered' ELSE 'hung up' END), count(*)
		FROM call_log WHERE started_at >= $2 AND ($1::uuid IS NULL OR owner_user_id = $1)
		GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 12`, sc.User, s.now().AddDate(0, 0, -days))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reason
	for rows.Next() {
		var r Reason
		if err := rows.Scan(&r.Reason, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CellUse is the calls one cell carried.
type CellUse struct {
	CellID      uuid.UUID
	Name        string
	Calls       int
	TalkSeconds int64
}

// PerCell returns the calls each cell carried in the last days. A call that
// moved between cells counts for each.
func (s *Store) PerCell(ctx context.Context, sc Scope, days int) ([]CellUse, error) {
	rows, err := s.Pool.Query(ctx, `SELECT ce.id, ce.name, count(*),
		coalesce(sum(extract(epoch FROM c.ended_at - c.answered_at)) FILTER (WHERE c.answered_at IS NOT NULL AND c.ended_at IS NOT NULL), 0)::bigint
		FROM call_log c CROSS JOIN LATERAL unnest(c.cell_ids) AS u(cid) JOIN cell ce ON ce.id = u.cid
		WHERE c.started_at >= $2 AND ($1::uuid IS NULL OR c.owner_user_id = $1)
		GROUP BY ce.id, ce.name ORDER BY 3 DESC, 2`, sc.User, s.now().AddDate(0, 0, -days))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CellUse
	for rows.Next() {
		var u CellUse
		if err := rows.Scan(&u.CellID, &u.Name, &u.Calls, &u.TalkSeconds); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Fleet counts the controller's cells by state and handsets by last contact;
// admins only (the caller checks).
type Fleet struct {
	CellsActive, CellsPending, CellsDisabled int
	Users, UsersPending                      int
	HandsetsSeen24h, HandsetsNeverSeen       int
	CertsExpiring                            int // active certificates ending within 14 days
}

// Fleet returns the fleet counts.
func (s *Store) Fleet(ctx context.Context) (Fleet, error) {
	var f Fleet
	now := s.now()
	err := s.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM cell WHERE state = 'active'),
		(SELECT count(*) FROM cell WHERE state = 'pending'),
		(SELECT count(*) FROM cell WHERE state = 'disabled'),
		(SELECT count(*) FROM app_user WHERE NOT disabled),
		(SELECT count(*) FROM app_user WHERE pending_approval),
		(SELECT count(*) FROM handset WHERE last_seen_at >= $1),
		(SELECT count(*) FROM handset WHERE last_seen_at IS NULL),
		(SELECT count(DISTINCT k.cell_id) FROM cell_certificate k JOIN cell c ON c.id = k.cell_id
			WHERE k.revoked_at IS NULL AND c.state = 'active' AND k.not_after < $2
			AND NOT EXISTS (SELECT 1 FROM cell_certificate n WHERE n.cell_id = k.cell_id AND n.revoked_at IS NULL AND n.not_after >= $2))`,
		now.Add(-24*time.Hour), now.AddDate(0, 0, 14)).
		Scan(&f.CellsActive, &f.CellsPending, &f.CellsDisabled, &f.Users, &f.UsersPending, &f.HandsetsSeen24h, &f.HandsetsNeverSeen, &f.CertsExpiring)
	return f, err
}

// Station is the numbers of one base station.
type Station struct {
	HandsetsSeen24h int   // handsets last seen on this cell in the last day
	Calls7d         int   // calls it carried in the last 7 days
	TalkSeconds7d   int64 // conversation time on it in the last 7 days
}

// Station returns one cell's numbers. The caller checks that the user may
// see this cell.
func (s *Store) Station(ctx context.Context, cellID uuid.UUID) (Station, error) {
	var st Station
	now := s.now()
	err := s.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM handset WHERE last_seen_cell_id = $1 AND last_seen_at >= $2),
		count(*), coalesce(sum(extract(epoch FROM ended_at - answered_at)) FILTER (WHERE answered_at IS NOT NULL AND ended_at IS NOT NULL), 0)::bigint
		FROM call_log WHERE $1 = ANY(cell_ids) AND started_at >= $3`,
		cellID, now.Add(-24*time.Hour), now.AddDate(0, 0, -7)).Scan(&st.HandsetsSeen24h, &st.Calls7d, &st.TalkSeconds7d)
	return st, err
}
