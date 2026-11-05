// SPDX-License-Identifier: AGPL-3.0-or-later

package stats

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// A call that has no end is one the controller lost track of (a restart in the middle of it); after this long it is not counted as
// still going on (calllog.ActiveWindow is the same figure).
const openCallWindow = 6 * time.Hour

// PoorMOS is the value below which a call's quality is called poor (ITU-T G.107: 3.5 and above is acceptable).
const PoorMOS = 3.5

// localDays returns the local days of the last n, oldest first, with their bounds and the calendar date: the bounds are worked out
// here, not in SQL, because the database session's zone is another one (see Days).
func localDays(now time.Time, n int) (dates, starts, ends []time.Time) {
	first := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(n - 1))
	for i := range n {
		d := first.AddDate(0, 0, i)
		dates = append(dates, time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC))
		starts = append(starts, d)
		ends = append(ends, d.AddDate(0, 0, 1))
	}
	return dates, starts, ends
}

// Heat is the calls of a period by weekday (Monday first) and hour of the day in the controller's time zone.
type Heat struct {
	Cells [7][24]int
	Max   int
	Total int
}

// Heat counts the calls started in the last days by weekday and hour. The database groups them by the hour (in UTC); the hours are
// then put on the local clock, so daylight saving time is right for zones that differ from UTC by whole hours.
func (s *Store) Heat(ctx context.Context, sc Scope, days int) (Heat, error) {
	now := s.now()
	since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	rows, err := s.Pool.Query(ctx, `SELECT date_trunc('hour', started_at AT TIME ZONE 'UTC'), count(*) FROM call_log
		WHERE started_at >= $2 AND ($1::uuid IS NULL OR owner_user_id = $1) GROUP BY 1`, sc.User, since)
	if err != nil {
		return Heat{}, err
	}
	defer rows.Close()
	var h Heat
	for rows.Next() {
		var hour time.Time // a timestamp without zone, read as UTC
		var n int
		if err := rows.Scan(&hour, &n); err != nil {
			return Heat{}, err
		}
		local := time.Date(hour.Year(), hour.Month(), hour.Day(), hour.Hour(), 0, 0, 0, time.UTC).In(now.Location())
		wd := (int(local.Weekday()) + 6) % 7 // Monday is 0
		h.Cells[wd][local.Hour()] += n
		h.Total += n
	}
	for _, row := range h.Cells {
		for _, n := range row {
			h.Max = max(h.Max, n)
		}
	}
	return h, rows.Err()
}

// Peak is the most calls that were going on at once on one day.
type Peak struct {
	Day  time.Time
	Peak int
}

// peakEvents is the events of the calls going on in the window: +1 where one starts, -1 where it ends. A call without an end that
// began a long time ago is left out.
const peakEvents = `SELECT c.started_at AS t, 1 AS d, c.cell_ids FROM call_log c
		WHERE c.started_at < $3 AND (c.ended_at >= $2 OR (c.ended_at IS NULL AND c.started_at >= $4)) AND ($1::uuid IS NULL OR c.owner_user_id = $1)
	UNION ALL
	SELECT c.ended_at, -1, c.cell_ids FROM call_log c
		WHERE c.started_at < $3 AND c.ended_at >= $2 AND ($1::uuid IS NULL OR c.owner_user_id = $1)`

// Peaks returns, for each of the last days (oldest first), the most calls going on at once on the whole controller (or the scope's).
// A call counts from its start to its end; one that crosses midnight counts on both days.
func (s *Store) Peaks(ctx context.Context, sc Scope, n int) ([]Peak, error) {
	now := s.now()
	dates, starts, ends := localDays(now, n)
	// the running number of calls going on after each event, and the number at the first moment of each day
	rows, err := s.Pool.Query(ctx, `WITH ev AS (`+peakEvents+`),
		run AS (SELECT t, sum(d) OVER (ORDER BY t, d ROWS UNBOUNDED PRECEDING) AS c FROM ev)
		SELECT t.day, coalesce(max(r.c), 0), coalesce((SELECT r2.c FROM run r2 WHERE r2.t < t.s ORDER BY r2.t DESC, r2.c LIMIT 1), 0)
		FROM unnest($5::date[], $6::timestamptz[], $7::timestamptz[]) AS t(day, s, e) LEFT JOIN run r ON r.t >= t.s AND r.t < t.e
		GROUP BY t.day, t.s ORDER BY t.day`,
		sc.User, starts[0], ends[len(ends)-1], now.Add(-openCallWindow), dates, starts, ends)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Peak
	for rows.Next() {
		var p Peak
		var inDay, carried int
		if err := rows.Scan(&p.Day, &inDay, &carried); err != nil {
			return nil, err
		}
		p.Peak = max(inDay, carried)
		out = append(out, p)
	}
	return out, rows.Err()
}

// CellPeak is the most calls a base station carried at once in the period, and what it can carry.
type CellPeak struct {
	CellID   uuid.UUID
	Name     string
	Peak     int
	Capacity int // calls at once the base station says it can carry; 0: it has not said
}

// PeakPerCell returns each base station's peak number of calls at once in the last days, highest first. Only calls that were still going on
// when the first day began or began later are looked at (peakEvents), so every event before that day is the start of a call that was alive at its
// beginning, and the running count there only rises to the number carried into the day: the maximum over all events is the maximum in the days. A call that moved between
// base stations counts on each of them for the whole call, as PerCell does.
func (s *Store) PeakPerCell(ctx context.Context, sc Scope, days int) ([]CellPeak, error) {
	now := s.now()
	_, starts, ends := localDays(now, days)
	rows, err := s.Pool.Query(ctx, `WITH ev AS (`+peakEvents+`),
		cev AS (SELECT t, d, u.cid FROM ev CROSS JOIN LATERAL unnest(ev.cell_ids) AS u(cid)),
		run AS (SELECT cid, t, sum(d) OVER (PARTITION BY cid ORDER BY t, d ROWS UNBOUNDED PRECEDING) AS c FROM cev)
		SELECT ce.id, ce.name, max(r.c)::int,
			coalesce(ce.cap_audio_nb, 0)::int
		FROM run r JOIN cell ce ON ce.id = r.cid GROUP BY ce.id, ce.name, ce.cap_audio_nb ORDER BY 3 DESC, 2`,
		sc.User, starts[0], ends[len(ends)-1], now.Add(-openCallWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CellPeak
	for rows.Next() {
		var p CellPeak
		if err := rows.Scan(&p.CellID, &p.Name, &p.Peak, &p.Capacity); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Usage is what one VoIP line or one handset did in the period.
type Usage struct {
	ID          uuid.UUID
	Name        string // "" for what was removed since
	Calls       int
	Answered    int
	Missed      int // incoming calls nobody answered
	TalkSeconds int64
	MOSCalls    int     // calls with a quality measure
	AvgMOS      float64 // their average
	Poor        int     // of them, below PoorMOS
}

// PerLine returns what each VoIP line did in the last days, busiest first. Calls of a line that has since been removed are one row
// without a name (ID is the nil UUID).
func (s *Store) PerLine(ctx context.Context, sc Scope, days int) ([]Usage, error) {
	return s.usage(ctx, sc, days, "sip_account_id", "sip_account", "label")
}

// PerHandset is PerLine for handsets.
func (s *Store) PerHandset(ctx context.Context, sc Scope, days int) ([]Usage, error) {
	return s.usage(ctx, sc, days, "handset_id", "handset", "display_name")
}

func (s *Store) usage(ctx context.Context, sc Scope, days int, col, table, nameCol string) ([]Usage, error) {
	// col, table and nameCol are constants of this file, never input
	rows, err := s.Pool.Query(ctx, `SELECT coalesce(c.`+col+`, '00000000-0000-0000-0000-000000000000'::uuid), coalesce(max(t.`+nameCol+`), ''),
			count(*), count(*) FILTER (WHERE c.answered_at IS NOT NULL),
			count(*) FILTER (WHERE c.direction = 'in' AND c.answered_at IS NULL AND c.ended_at IS NOT NULL),
			coalesce(sum(extract(epoch FROM c.ended_at - c.answered_at)) FILTER (WHERE c.answered_at IS NOT NULL AND c.ended_at IS NOT NULL), 0)::bigint,
			count(*) FILTER (WHERE (c.quality->>'mos') IS NOT NULL), coalesce(avg((c.quality->>'mos')::float8), 0),
			count(*) FILTER (WHERE (c.quality->>'mos')::float8 < $3)
		FROM call_log c LEFT JOIN `+table+` t ON t.id = c.`+col+`
		WHERE c.started_at >= $2 AND ($1::uuid IS NULL OR c.owner_user_id = $1)
		GROUP BY c.`+col+` ORDER BY 3 DESC, 2`, sc.User, s.now().AddDate(0, 0, -days), PoorMOS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Usage
	for rows.Next() {
		var u Usage
		if err := rows.Scan(&u.ID, &u.Name, &u.Calls, &u.Answered, &u.Missed, &u.TalkSeconds, &u.MOSCalls, &u.AvgMOS, &u.Poor); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// QualityDay is a day's call quality.
type QualityDay struct {
	Day    time.Time
	Calls  int // calls with a quality measure
	AvgMOS float64
	Poor   int
}

// Quality returns the last days' call quality (the average MOS of the calls that were measured, and how many were poor), oldest first.
func (s *Store) Quality(ctx context.Context, sc Scope, n int) ([]QualityDay, error) {
	dates, starts, ends := localDays(s.now(), n)
	rows, err := s.Pool.Query(ctx, `SELECT t.day, count(c.id), coalesce(avg((c.quality->>'mos')::float8), 0), count(c.id) FILTER (WHERE (c.quality->>'mos')::float8 < $5)
		FROM unnest($2::date[], $3::timestamptz[], $4::timestamptz[]) AS t(day, s, e)
		LEFT JOIN call_log c ON c.started_at >= t.s AND c.started_at < t.e AND (c.quality->>'mos') IS NOT NULL AND ($1::uuid IS NULL OR c.owner_user_id = $1)
		GROUP BY t.day ORDER BY t.day`, sc.User, dates, starts, ends, PoorMOS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QualityDay
	for rows.Next() {
		var d QualityDay
		if err := rows.Scan(&d.Day, &d.Calls, &d.AvgMOS, &d.Poor); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// WorstCall is one call with a poor quality measure.
type WorstCall struct {
	ID      int64
	Started time.Time
	MOS     float64
}

// Worst returns the n calls with the lowest MOS of the last days, the worst first, only those below PoorMOS.
func (s *Store) Worst(ctx context.Context, sc Scope, days, n int) ([]WorstCall, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, started_at, (quality->>'mos')::float8 FROM call_log
		WHERE started_at >= $2 AND (quality->>'mos') IS NOT NULL AND (quality->>'mos')::float8 < $4 AND ($1::uuid IS NULL OR owner_user_id = $1)
		ORDER BY 3, id DESC LIMIT $3`, sc.User, s.now().AddDate(0, 0, -days), n, PoorMOS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorstCall
	for rows.Next() {
		var w WorstCall
		if err := rows.Scan(&w.ID, &w.Started, &w.MOS); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Period is the headline numbers of a stretch of days, to set against the one before it.
type Period struct {
	Calls       int
	Answered    int
	Missed      int
	Failed      int // outgoing calls that never reached a conversation
	TalkSeconds int64
}

// Compare returns the last n days and the n days before them.
func (s *Store) Compare(ctx context.Context, sc Scope, n int) (cur, prev Period, err error) {
	now := s.now()
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	mid, start := end.AddDate(0, 0, -n), end.AddDate(0, 0, -2*n)
	read := func(from, to time.Time) (p Period, err error) {
		err = s.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE answered_at IS NOT NULL),
				count(*) FILTER (WHERE direction = 'in' AND answered_at IS NULL AND ended_at IS NOT NULL),
				count(*) FILTER (WHERE direction = 'out' AND answered_at IS NULL),
				coalesce(sum(extract(epoch FROM ended_at - answered_at)) FILTER (WHERE answered_at IS NOT NULL AND ended_at IS NOT NULL), 0)::bigint
			FROM call_log WHERE started_at >= $2 AND started_at < $3 AND ($1::uuid IS NULL OR owner_user_id = $1)`, sc.User, from, to).
			Scan(&p.Calls, &p.Answered, &p.Missed, &p.Failed, &p.TalkSeconds)
		return p, err
	}
	if cur, err = read(mid, end); err != nil {
		return
	}
	prev, err = read(start, mid)
	return
}

// UserUsage is what one user did in a month, for the usage report.
type UserUsage struct {
	UserID      uuid.UUID
	Username    string
	DisplayName string
	Calls       int
	Answered    int
	TalkSeconds int64
	Handsets    int // the user has now
	Lines       int // the user has now
}

// OrgUsage is the usage of the members of one organization (a user in two organizations is in both).
type OrgUsage struct {
	OrgID       uuid.UUID
	Name        string
	Members     int
	Calls       int
	Answered    int
	TalkSeconds int64
	Handsets    int
	Lines       int
}

// MonthlyUsage returns the calls of each user whose calls started in the local month that begins at monthStart, with how many handsets
// and lines each has now; users without calls and without equipment are left out. For administrators who host others.
func (s *Store) MonthlyUsage(ctx context.Context, monthStart time.Time) ([]UserUsage, error) {
	to := monthStart.AddDate(0, 1, 0)
	rows, err := s.Pool.Query(ctx, `SELECT u.id, u.username, u.display_name, coalesce(c.calls, 0), coalesce(c.answered, 0), coalesce(c.talk, 0),
			(SELECT count(*) FROM handset h WHERE h.owner_user_id = u.id), (SELECT count(*) FROM sip_account a WHERE a.owner_user_id = u.id)
		FROM app_user u LEFT JOIN (SELECT owner_user_id, count(*) AS calls, count(*) FILTER (WHERE answered_at IS NOT NULL) AS answered,
				coalesce(sum(extract(epoch FROM ended_at - answered_at)) FILTER (WHERE answered_at IS NOT NULL AND ended_at IS NOT NULL), 0)::bigint AS talk
			FROM call_log WHERE started_at >= $1 AND started_at < $2 GROUP BY owner_user_id) c ON c.owner_user_id = u.id
		WHERE c.calls > 0 OR EXISTS (SELECT 1 FROM handset h WHERE h.owner_user_id = u.id) OR EXISTS (SELECT 1 FROM sip_account a WHERE a.owner_user_id = u.id)
		ORDER BY coalesce(c.calls, 0) DESC, lower(u.username)`, monthStart, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserUsage
	for rows.Next() {
		var u UserUsage
		if err := rows.Scan(&u.UserID, &u.Username, &u.DisplayName, &u.Calls, &u.Answered, &u.TalkSeconds, &u.Handsets, &u.Lines); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// OrgMonthlyUsage sums MonthlyUsage by organization.
func (s *Store) OrgMonthlyUsage(ctx context.Context, monthStart time.Time) ([]OrgUsage, error) {
	to := monthStart.AddDate(0, 1, 0)
	rows, err := s.Pool.Query(ctx, `SELECT o.id, o.name, count(DISTINCT m.user_id), coalesce(sum(c.calls), 0)::int, coalesce(sum(c.answered), 0)::int, coalesce(sum(c.talk), 0)::bigint,
			(SELECT count(*) FROM handset h JOIN organization_member m2 ON m2.user_id = h.owner_user_id WHERE m2.org_id = o.id),
			(SELECT count(*) FROM sip_account a JOIN organization_member m3 ON m3.user_id = a.owner_user_id WHERE m3.org_id = o.id)
		FROM organization o JOIN organization_member m ON m.org_id = o.id
		LEFT JOIN (SELECT owner_user_id, count(*) AS calls, count(*) FILTER (WHERE answered_at IS NOT NULL) AS answered,
				coalesce(sum(extract(epoch FROM ended_at - answered_at)) FILTER (WHERE answered_at IS NOT NULL AND ended_at IS NOT NULL), 0)::bigint AS talk
			FROM call_log WHERE started_at >= $1 AND started_at < $2 GROUP BY owner_user_id) c ON c.owner_user_id = m.user_id
		GROUP BY o.id, o.name ORDER BY 4 DESC, lower(o.name)`, monthStart, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgUsage
	for rows.Next() {
		var o OrgUsage
		if err := rows.Scan(&o.OrgID, &o.Name, &o.Members, &o.Calls, &o.Answered, &o.TalkSeconds, &o.Handsets, &o.Lines); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
