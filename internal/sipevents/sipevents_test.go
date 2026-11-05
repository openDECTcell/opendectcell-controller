// SPDX-License-Identifier: AGPL-3.0-or-later

package sipevents_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sipevents"
)

var ctx = context.Background()

func st(state sip.RegState, msg string) sip.RegStatus {
	return sip.RegStatus{State: state, LastError: msg}
}

func account(t *testing.T, r *sipevents.Recorder) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := r.Pool.QueryRow(ctx, "INSERT INTO sip_account (label, username, domain) VALUES ('x', 'u', 'd.example') RETURNING id").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRecorderWritesChangesNotRetries(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	r := &sipevents.Recorder{Pool: dbtest.Migrated(t), Now: func() time.Time { return now }}
	id := account(t, r)
	step := func(s sip.RegStatus, took time.Duration) {
		now = now.Add(time.Minute)
		r.Note(id, s, took)
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	step(st(sip.RegRegistering, ""), 0)                                // a step, not a result: nothing
	step(st(sip.RegFailed, "403 Forbidden"), 0)                        // 1: down
	step(st(sip.RegRegistering, ""), 0)                                //
	step(st(sip.RegFailed, "403 Forbidden"), 0)                        // the same again: nothing
	step(st(sip.RegBackoff, "timeout"), 0)                             // 2: down for another reason
	step(st(sip.RegBackoff, "timeout"), 0)                             // nothing
	step(st(sip.RegRegistered, "403 Forbidden"), 240*time.Millisecond) // 3: up, with its time (the error of before is stale)
	step(st(sip.RegRegistering, ""), 0)                                //
	step(st(sip.RegRegistered, ""), 90*time.Millisecond)               // a refresh that flipped the state: nothing
	step(st(sip.RegDisabled, ""), 0)                                   // 4: switched off
	ev, err := sipevents.Recent(ctx, r.Pool, id, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 4 {
		t.Fatalf("%d events: %+v", len(ev), ev)
	}
	// newest first
	if ev[0].State != sip.RegDisabled || ev[1].State != sip.RegRegistered || ev[1].Took != 240*time.Millisecond ||
		ev[2].State != sip.RegBackoff || ev[2].Error != "timeout" || ev[3].State != sip.RegFailed || ev[3].Error != "403 Forbidden" {
		t.Errorf("events: %+v", ev)
	}
	if ev[0].Error != "" || ev[1].Error != "" {
		t.Errorf("an error text on a line that is up or off: %+v", ev[:2])
	}

	// after a restart (a new recorder, nothing in memory) the database says what was last: off, so up is news; up again is not
	r2 := &sipevents.Recorder{Pool: r.Pool, Now: func() time.Time { return now }}
	for _, s := range []sip.RegStatus{st(sip.RegRegistered, ""), st(sip.RegRegistered, "")} {
		now = now.Add(time.Minute)
		r2.Note(id, s, 0)
		if err := r2.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if ev, _ = sipevents.Recent(ctx, r.Pool, id, 50); len(ev) != 5 || ev[0].State != sip.RegRegistered {
		t.Errorf("after the restart: %+v", ev)
	}
	// and a recorder that starts with the same state as the database's last says nothing
	r3 := &sipevents.Recorder{Pool: r.Pool, Now: func() time.Time { return now }}
	r3.Note(id, st(sip.RegRegistered, ""), 0)
	_ = r3.Flush(ctx)
	if ev, _ = sipevents.Recent(ctx, r.Pool, id, 50); len(ev) != 5 {
		t.Errorf("a restart with the same state added a row: %d", len(ev))
	}
}

func TestRecorderBoundsAndSurvives(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	r := &sipevents.Recorder{Pool: dbtest.Migrated(t), Now: func() time.Time { return now }}
	id := account(t, r)
	r.Note(id, st(sip.RegFailed, strings.Repeat("x", 1000)+"\xff\xfe"), 0)
	// a line that no longer exists: nothing recorded, no error
	r.Note(uuid.New(), st(sip.RegFailed, "gone"), 0)
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	ev, _ := sipevents.Recent(ctx, r.Pool, id, 5)
	if len(ev) != 1 || len(ev[0].Error) != 300 {
		t.Fatalf("a long error: %d events, %d bytes", len(ev), len(ev[0].Error))
	}
	var n int
	r.Pool.QueryRow(ctx, "SELECT count(*) FROM sip_reg_event").Scan(&n)
	if n != 1 {
		t.Errorf("%d rows", n)
	}
	// the report never blocks, whatever the writer does
	done := make(chan struct{})
	go func() {
		for range 5000 {
			r.Note(id, st(sip.RegFailed, "e"), 0)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Note blocked with nobody writing")
	}
	// removing the line removes its history
	r.Pool.Exec(ctx, "DELETE FROM sip_account WHERE id = $1", id)
	if ev, _ = sipevents.Recent(ctx, r.Pool, id, 5); len(ev) != 0 {
		t.Errorf("history of a deleted line: %d", len(ev))
	}
}

func TestSummarise(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	h := func(ago float64) time.Time { return now.Add(-time.Duration(ago * float64(time.Hour))) }
	// newest first
	ev := []sipevents.Event{
		{At: h(1), State: sip.RegRegistered, Took: 200 * time.Millisecond},
		{At: h(3), State: sip.RegBackoff, Error: "timeout"},
		{At: h(4), State: sip.RegFailed, Error: "403"}, // a change of reason: the same fall
		{At: h(5), State: sip.RegRegistered, Took: 400 * time.Millisecond},
		{At: h(30), State: sip.RegRegistered, Took: 5 * time.Second}, // before the window: says what it began in
	}
	s := sipevents.Summarise(ev, now, 24*time.Hour)
	// inside the 24 h window: up from -24 to -4 (20 h), down from -4 to -1 (3 h), up from -1 to now (1 h): 21 h of 24
	if !s.HasData || s.Counted != 24*time.Hour || s.UpPercent < 87.4 || s.UpPercent > 87.6 {
		t.Errorf("percent: %+v", s)
	}
	if s.Downs != 1 {
		t.Errorf("downs: %d", s.Downs)
	}
	if s.AvgTook != 300*time.Millisecond { // 200 and 400 inside the window, not the 5 s before it
		t.Errorf("average time: %v", s.AvgTook)
	}
	if s.LastUp == nil || !s.LastUp.Equal(h(1)) || s.LastDown == nil || !s.LastDown.Equal(h(3)) {
		t.Errorf("last up/down: %v %v", s.LastUp, s.LastDown)
	}
	// time switched off is not time down; unknown time is not counted
	ev = []sipevents.Event{{At: h(2), State: sip.RegRegistered}, {At: h(10), State: sip.RegDisabled}}
	s = sipevents.Summarise(ev, now, 24*time.Hour)
	if s.Counted != 2*time.Hour || s.UpPercent != 100 {
		t.Errorf("off time counted: %+v", s)
	}
	if s := sipevents.Summarise(nil, now, time.Hour); s.HasData {
		t.Error("data from nothing")
	}
}

func TestWindowQuery(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	r := &sipevents.Recorder{Pool: dbtest.Migrated(t)}
	id := account(t, r)
	for _, e := range []struct {
		ago   time.Duration
		state string
	}{{100 * time.Hour, "registered"}, {50 * time.Hour, "failed"}, {10 * time.Hour, "registered"}, {2 * time.Hour, "failed"}} {
		r.Pool.Exec(ctx, "INSERT INTO sip_reg_event (account_id, at, state) VALUES ($1, $2, $3)", id, now.Add(-e.ago), e.state)
	}
	ev, err := sipevents.Window(ctx, r.Pool, id, now, 24*time.Hour)
	if err != nil || len(ev) != 3 || ev[2].State != sip.RegFailed || ev[0].State != sip.RegFailed {
		t.Fatalf("window: %v %+v", err, ev) // the two inside, and the one just before (failed 50 h ago), not the one before that
	}
}
