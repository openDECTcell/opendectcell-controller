// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sipevents keeps the history of each VoIP line's registration (ideas B3): when it went up, when it went down and why,
// how long the registration took. The SIP side reports every change of state or error; this package writes a row only when
// something a person would want to read changed (a line that keeps failing the same way is one row, not one per retry).
package sipevents

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/sip"
)

// KeepDays is how long the history is kept (the retention job deletes older rows).
const KeepDays = 90

// maxError bounds the error text kept per row.
const maxError = 300

// Event is one row of a line's history.
type Event struct {
	At    time.Time
	State sip.RegState // registered, failed, backoff or disabled
	Error string
	Took  time.Duration // for a registration: how long the attempt took; 0 if not known
}

// Class is what a state means to a reader: up, down (failed or waiting to retry) or off (the line is switched off).
func Class(s sip.RegState) string {
	switch s {
	case sip.RegRegistered:
		return "up"
	case sip.RegFailed, sip.RegBackoff:
		return "down"
	case sip.RegDisabled:
		return "off"
	}
	return ""
}

type change struct {
	account uuid.UUID
	st      sip.RegStatus
	took    time.Duration
	at      time.Time
}

type last struct {
	class, err string
}

// Recorder takes the SIP side's reports and writes the history. Note is safe to call from the SIP lines: it never blocks.
type Recorder struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	Now  func() time.Time

	in   chan change
	once sync.Once
	mu   sync.Mutex
	seen map[uuid.UUID]last
}

func (r *Recorder) init() {
	r.once.Do(func() {
		r.in = make(chan change, 256)
		r.seen = map[uuid.UUID]last{}
		if r.Now == nil {
			r.Now = time.Now
		}
		if r.Log == nil {
			r.Log = slog.Default()
		}
	})
}

// Note is sip.Config.OnRegChange. A report is dropped when the writer is far behind: history is not worth stalling a registration.
func (r *Recorder) Note(account uuid.UUID, st sip.RegStatus, took time.Duration) {
	r.init()
	select {
	case r.in <- change{account, st, took, r.Now()}:
	default:
	}
}

// Run writes the reports until ctx ends.
func (r *Recorder) Run(ctx context.Context) {
	r.init()
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-r.in:
			if err := r.handle(ctx, c); err != nil && ctx.Err() == nil {
				r.Log.Warn("registration history", "err", err)
			}
		}
	}
}

// Flush writes what has been reported and not yet written (tests, shutdown).
func (r *Recorder) Flush(ctx context.Context) error {
	r.init()
	for {
		select {
		case c := <-r.in:
			if err := r.handle(ctx, c); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (r *Recorder) handle(ctx context.Context, c change) error {
	class := Class(c.st.State)
	if class == "" { // registering: a step, not a result
		return nil
	}
	errText := c.st.LastError
	if class != "down" {
		errText = ""
	}
	if len(errText) > maxError {
		errText = errText[:maxError]
	}
	errText = strings.ToValidUTF8(errText, "")
	prev, ok, err := r.previous(ctx, c.account)
	if err != nil {
		return err
	}
	if ok && prev.class == class && (class != "down" || prev.err == errText) {
		return nil
	}
	var took *int32
	if class == "up" && c.took > 0 {
		ms := int32(min(c.took.Milliseconds(), 1<<30))
		took = &ms
	}
	_, err = r.Pool.Exec(ctx, "INSERT INTO sip_reg_event (account_id, at, state, error, took_ms) VALUES ($1, $2, $3, $4, $5)",
		c.account, c.at, string(c.st.State), errText, took)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" {
		return nil // the line was deleted while its change was on the way: nothing to record
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.seen[c.account] = last{class, errText}
	r.mu.Unlock()
	return nil
}

// previous is the last class and error recorded for the account: from memory, else from the database.
func (r *Recorder) previous(ctx context.Context, id uuid.UUID) (last, bool, error) {
	r.mu.Lock()
	p, ok := r.seen[id]
	r.mu.Unlock()
	if ok {
		return p, true, nil
	}
	var state, errText string
	err := r.Pool.QueryRow(ctx, "SELECT state, error FROM sip_reg_event WHERE account_id = $1 ORDER BY at DESC, id DESC LIMIT 1", id).Scan(&state, &errText)
	if errors.Is(err, pgx.ErrNoRows) {
		return last{}, false, nil
	}
	if err != nil {
		return last{}, false, err
	}
	p = last{Class(sip.RegState(state)), errText}
	r.mu.Lock()
	r.seen[id] = p
	r.mu.Unlock()
	return p, true, nil
}

func scanEvent(r pgx.CollectableRow) (Event, error) {
	var e Event
	var st string
	var ms int
	err := r.Scan(&e.At, &st, &e.Error, &ms)
	e.State, e.Took = sip.RegState(st), time.Duration(ms)*time.Millisecond
	return e, err
}

// Recent returns the newest events of the account, newest first.
func Recent(ctx context.Context, pool *pgxpool.Pool, account uuid.UUID, limit int) ([]Event, error) {
	rows, err := pool.Query(ctx, "SELECT at, state, error, coalesce(took_ms, 0) FROM sip_reg_event WHERE account_id = $1 ORDER BY at DESC, id DESC LIMIT $2", account, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanEvent)
}

// Window returns the events that decide a Summary over the window ending at now, newest first: those inside it and the one before it.
func Window(ctx context.Context, pool *pgxpool.Pool, account uuid.UUID, now time.Time, window time.Duration) ([]Event, error) {
	rows, err := pool.Query(ctx, `SELECT at, state, error, coalesce(took_ms, 0) FROM sip_reg_event
		WHERE account_id = $1 AND (at >= $2 OR id = (SELECT id FROM sip_reg_event WHERE account_id = $1 AND at < $2 ORDER BY at DESC, id DESC LIMIT 1))
		ORDER BY at DESC, id DESC`, account, now.Add(-window))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanEvent)
}

// Summary is what the history says about a window of time.
type Summary struct {
	HasData   bool          // there is a history to read
	UpPercent float64       // of the time the line was on (not disabled), how much it was registered
	Counted   time.Duration // the time the percentage is taken over
	Downs     int           // how many times it went down in the window
	AvgTook   time.Duration // the average registration time in the window; 0 if none is known
	LastUp    *time.Time
	LastDown  *time.Time
}

// Hours is the time the percentage is taken over, in hours.
func (s Summary) Hours() float64 { return s.Counted.Hours() }

// Summarise works out a Summary from the events of Window (newest first) for the window that ends at now. Time before the first
// event is unknown and is not counted; time the line was disabled is not counted either.
func Summarise(newestFirst []Event, now time.Time, window time.Duration) Summary {
	var s Summary
	n := len(newestFirst)
	if n == 0 {
		return s
	}
	s.HasData = true
	since := now.Add(-window)
	var up, down, took time.Duration
	var nTook int
	for i := n - 1; i >= 0; i-- { // oldest first
		e := newestFirst[i]
		class := Class(e.State)
		end := now
		if i > 0 {
			end = newestFirst[i-1].At
		}
		start := e.At
		if start.Before(since) {
			start = since
		}
		if end.After(start) {
			switch class {
			case "up":
				up += end.Sub(start)
			case "down":
				down += end.Sub(start)
			}
		}
		at := e.At
		switch class {
		case "up":
			s.LastUp = &at
		case "down":
			s.LastDown = &at
		}
		if e.At.Before(since) {
			continue // the event before the window only says what state it began in
		}
		if class == "down" && (i == n-1 || Class(newestFirst[i+1].State) != "down") {
			s.Downs++ // a change of reason while down is not a new fall
		}
		if class == "up" && e.Took > 0 {
			took += e.Took
			nTook++
		}
	}
	s.Counted = up + down
	if s.Counted > 0 {
		s.UpPercent = 100 * float64(up) / float64(s.Counted)
	}
	if nTook > 0 {
		s.AvgTook = took / time.Duration(nTook)
	}
	return s
}
