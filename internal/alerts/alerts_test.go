// SPDX-License-Identifier: AGPL-3.0-or-later

package alerts_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/alerts"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
)

var ctx = context.Background()

func user(t *testing.T, pool *pgxpool.Pool, name, role string, prefs string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, "INSERT INTO app_user (username, role, auth_source, email, alert_prefs) VALUES ($1, $2, 'local', $1 || '@example.org', $3::jsonb) RETURNING id", name, role, prefs).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func notes(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT level || ':' || subject FROM notification WHERE user_id = $1 ORDER BY id", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 5, h, m, 0, 0, time.Local) }
	night := alerts.Prefs{QuietFrom: "22:00", QuietTo: "07:00"}
	day := alerts.Prefs{QuietFrom: "09:00", QuietTo: "17:30"}
	for _, c := range []struct {
		p    alerts.Prefs
		h, m int
		want bool
	}{{night, 23, 0, true}, {night, 3, 0, true}, {night, 6, 59, true}, {night, 7, 0, false}, {night, 21, 59, false}, {night, 22, 0, true}, {night, 12, 0, false},
		{day, 9, 0, true}, {day, 17, 29, true}, {day, 17, 30, false}, {day, 8, 59, false},
		{alerts.Prefs{}, 3, 0, false}, {alerts.Prefs{QuietFrom: "22:00"}, 23, 0, false}, {alerts.Prefs{QuietFrom: "08:00", QuietTo: "08:00"}, 8, 0, false},
		{alerts.Prefs{QuietFrom: "xx", QuietTo: "07:00"}, 3, 0, false}} {
		if got := c.p.Quiet(at(c.h, c.m)); got != c.want {
			t.Errorf("%+v at %d:%02d: %v", c.p, c.h, c.m, got)
		}
	}
	for s, ok := range map[string]bool{"": true, "07:30": true, "23:59": true, "24:00": false, "7": false, "12:60": false, "ab:cd": false} {
		if alerts.ValidClock(s) != ok {
			t.Errorf("ValidClock(%q) = %v", s, !ok)
		}
	}
	if p := alerts.ParsePrefs([]byte("{not json")); p.HandsetHours != 0 || !p.Shows("line") || p.Mails("line") {
		t.Errorf("damaged prefs are not the defaults: %+v", p)
	}
	p := alerts.ParsePrefs(alerts.Prefs{Kinds: map[string]alerts.KindPref{"line": {Hide: true, Mail: true}}, HandsetHours: 48}.JSON())
	if p.Shows("line") || !p.Mails("line") || !p.Shows("cell") || p.HandsetHours != 48 {
		t.Errorf("round trip: %+v", p)
	}
}

type probe struct {
	mu    sync.Mutex
	conds []alerts.Condition
	err   error
}

func (p *probe) set(c ...alerts.Condition) {
	p.mu.Lock()
	p.conds, p.err = c, nil
	p.mu.Unlock()
}

func (p *probe) source() alerts.Source {
	return alerts.Source{Kinds: []string{alerts.KindLine}, Find: func(context.Context, time.Time) ([]alerts.Condition, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.conds, p.err
	}}
}

func TestEngineTellsOnceAndTellsTheEnd(t *testing.T) {
	pool := dbtest.Migrated(t)
	anna, ben := user(t, pool, "anna", "user", `{}`), user(t, pool, "ben", "user", `{}`)
	admin := user(t, pool, "root", "admin", `{}`)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	p := &probe{}
	mk := func() *alerts.Engine {
		return &alerts.Engine{Pool: pool, Sources: []alerts.Source{p.source()}, Now: func() time.Time { return now }}
	}
	e := mk()
	bad := alerts.Condition{Kind: alerts.KindLine, Object: "l1", Who: alerts.Who{Users: []uuid.UUID{anna}}, After: 10 * time.Minute, Level: "danger",
		Title: "Line Home is down", Body: "403", Href: "/sip-accounts/1", OkTitle: "Line Home is back"}
	tick := func(d time.Duration) {
		now = now.Add(d)
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	fine := alerts.Condition{Status: alerts.Fine, Kind: alerts.KindLine, Object: "l1"}
	p.set(bad)
	tick(0)
	tick(9 * time.Minute)
	if n := notes(t, pool, anna); len(n) != 0 {
		t.Fatalf("told before it lasted 10 minutes: %v", n)
	}
	tick(2 * time.Minute) // 11 minutes
	if n := notes(t, pool, anna); len(n) != 1 || n[0] != "danger:Line Home is down" {
		t.Fatalf("after 11 minutes: %v", n)
	}
	// not told again, also not after a restart (a new engine, the state is in the database)
	tick(time.Minute)
	e = mk()
	tick(5 * time.Minute)
	if n := notes(t, pool, anna); len(n) != 1 {
		t.Fatalf("told again: %v", n)
	}
	// "in between" keeps the episode and its clock; it says nothing
	p.set(alerts.Condition{Status: alerts.Unsure, Kind: alerts.KindLine, Object: "l1"})
	tick(time.Minute)
	if n := notes(t, pool, anna); len(n) != 1 {
		t.Fatalf("unsure was told: %v", n)
	}
	// its end is told once
	p.set(fine)
	tick(time.Minute)
	tick(time.Minute)
	if n := notes(t, pool, anna); len(n) != 2 || n[1] != "ok:Line Home is back" {
		t.Fatalf("the end: %v", n)
	}
	if len(notes(t, pool, ben))+len(notes(t, pool, admin)) != 0 {
		t.Error("somebody else was told")
	}
	var left int
	pool.QueryRow(ctx, "SELECT count(*) FROM alert_state").Scan(&left)
	if left != 0 {
		t.Errorf("%d states left after the end", left)
	}

	// one that is over before it was told is never told, and not "back"
	p.set(bad)
	tick(0)
	p.set(fine)
	tick(time.Minute)
	p.set(bad)
	tick(0)
	if n := notes(t, pool, anna); len(n) != 2 {
		t.Fatalf("a short one was told: %v", n)
	}
	// the clock of a flap starts anew: 9 minutes of the second round is not 10
	tick(9 * time.Minute)
	if n := notes(t, pool, anna); len(n) != 2 {
		t.Fatalf("a flap kept its old clock: %v", n)
	}
}

func TestEngineAbsenceIsNotNewsWhenTheSourceFailed(t *testing.T) {
	pool := dbtest.Migrated(t)
	anna := user(t, pool, "anna", "user", `{}`)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	p := &probe{}
	e := &alerts.Engine{Pool: pool, Sources: []alerts.Source{p.source()}, Now: func() time.Time { return now }}
	p.set(alerts.Condition{Kind: alerts.KindLine, Object: "l1", Who: alerts.Who{Users: []uuid.UUID{anna}}, Level: "danger", Title: "down", OkTitle: "back"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.conds, p.err = nil, errors.New("the status source is down")
	p.mu.Unlock()
	if err := e.Tick(ctx); err == nil {
		t.Fatal("the failure of a source was not reported")
	}
	if n := notes(t, pool, anna); len(n) != 1 {
		t.Fatalf("a failed look was taken for the end: %v", n)
	}
	// and a source can only speak for its own kinds
	p.set(alerts.Condition{Kind: alerts.KindCert, Object: "x", Who: alerts.Who{Users: []uuid.UUID{anna}}, Title: "not mine"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	for _, line := range notes(t, pool, anna) {
		if strings.Contains(line, "not mine") {
			t.Fatalf("a source spoke for another kind: %v", notes(t, pool, anna))
		}
	}
	// the line is not mentioned any more (deleted, switched off): forgotten, and "back" is not said, because it did not get better
	if n := notes(t, pool, anna); len(n) != 1 {
		t.Fatalf("a line that vanished was reported as back: %v", n)
	}
	var left int
	pool.QueryRow(ctx, "SELECT count(*) FROM alert_state WHERE kind = 'line'").Scan(&left)
	if left != 0 {
		t.Errorf("the state of a line that is not watched is kept")
	}
}

func TestNotifyHonoursPreferences(t *testing.T) {
	pool := dbtest.Migrated(t)
	plain := user(t, pool, "plain", "user", `{}`)
	hidden := user(t, pool, "hidden", "user", `{"kinds":{"line":{"hide":true}}}`)
	mailOnly := user(t, pool, "mailonly", "user", `{"kinds":{"line":{"hide":true,"mail":true}}}`)
	both := user(t, pool, "both", "user", `{"kinds":{"line":{"mail":true}}}`)
	admin := user(t, pool, "root", "admin", `{}`)
	off := user(t, pool, "off", "user", `{}`)
	pool.Exec(ctx, "UPDATE app_user SET disabled = true WHERE id = $1", off)
	var sunk []uuid.UUID
	who := alerts.Who{Users: []uuid.UUID{plain, hidden, mailOnly, both, admin, off, plain}, Admins: true}
	if err := alerts.Notify(ctx, pool, who, alerts.Notification{Kind: alerts.KindLine, Level: "danger", Subject: "s", Body: "b"}, func(u uuid.UUID, _ alerts.Notification) { sunk = append(sunk, u) }); err != nil {
		t.Fatal(err)
	}
	count := func(id uuid.UUID, where string) int {
		var n int
		pool.QueryRow(ctx, "SELECT count(*) FROM notification WHERE user_id = $1 AND "+where, id).Scan(&n)
		return n
	}
	if count(plain, "read_at IS NULL AND NOT mail_wanted") != 1 || count(admin, "true") != 1 {
		t.Error("a user who has not chosen anything is not told once")
	}
	if count(hidden, "true") != 0 {
		t.Error("a user who hid the kind was told")
	}
	if count(mailOnly, "read_at IS NOT NULL AND mail_wanted") != 1 {
		t.Error("mail only: a read row that is wanted by mail")
	}
	if count(both, "read_at IS NULL AND mail_wanted") != 1 {
		t.Error("both: an unread row that is wanted by mail")
	}
	if count(off, "true") != 0 {
		t.Error("a disabled user was told")
	}
	if len(sunk) != 4 { // plain, mailOnly, both, admin
		t.Errorf("the sink was told %d times", len(sunk))
	}
}

type fakeMail struct {
	mu   sync.Mutex
	sent []string
	to   []string
	fail bool
	off  bool
}

func (f *fakeMail) Enabled() bool { return !f.off }
func (f *fakeMail) Send(_ context.Context, to []string, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("smtp down")
	}
	f.sent, f.to = append(f.sent, subject+"\n"+body), append(f.to, to...)
	return nil
}

func TestMailWaitsOutQuietHoursAndGivesUp(t *testing.T) {
	pool := dbtest.Migrated(t)
	quiet := user(t, pool, "quiet", "user", `{"kinds":{"line":{"mail":true}},"quiet_from":"22:00","quiet_to":"07:00"}`)
	loud := user(t, pool, "loud", "user", `{"kinds":{"line":{"mail":true}}}`)
	// the notifications are made "at" fake times, because the mail run counts a day from the fake clock
	n := func(id uuid.UUID, s string, at time.Time) {
		if err := alerts.Notify(ctx, pool, alerts.Who{Users: []uuid.UUID{id}}, alerts.Notification{Kind: alerts.KindLine, Level: "danger", Subject: s, Body: "body of " + s}, nil); err != nil {
			t.Fatal(err)
		}
		pool.Exec(ctx, "UPDATE notification SET at = $2 WHERE subject = $1", s, at)
	}
	evening := time.Date(2026, 11, 5, 20, 0, 0, 0, time.Local)
	n(quiet, "q1", evening)
	n(loud, "l1", evening)
	n(loud, "l2", evening)
	f := &fakeMail{}
	night := time.Date(2026, 11, 5, 23, 0, 0, 0, time.Local)
	m := &alerts.MailRun{Pool: pool, Mailer: f, Now: func() time.Time { return night }}
	if err := m.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.to[0] != "loud@example.org" || !strings.Contains(f.sent[0], "2 notifications") || !strings.Contains(f.sent[0], "body of l1") || !strings.Contains(f.sent[0], "l2") {
		t.Fatalf("at night: %q", f.sent)
	}
	// a second run sends nothing more; the quiet one waits
	if err := m.Once(ctx); err != nil || len(f.sent) != 1 {
		t.Fatalf("again: %v %d", err, len(f.sent))
	}
	// in the morning the quiet one gets it, as one mail with the subject of the notification
	m.Now = func() time.Time { return time.Date(2026, 11, 6, 7, 1, 0, 0, time.Local) }
	if err := m.Once(ctx); err != nil || len(f.sent) != 2 || f.to[1] != "quiet@example.org" || !strings.HasPrefix(f.sent[1], "openDECTcell: q1\n") {
		t.Fatalf("in the morning: %v %q", err, f.sent)
	}
	// a mail server that is down: kept for the next run; mail not set up: nothing happens
	n(loud, "l3", time.Date(2026, 11, 6, 11, 0, 0, 0, time.Local))
	f.fail = true
	m.Now = func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local) }
	if err := m.Once(ctx); err == nil {
		t.Fatal("a failed send was not reported")
	}
	f.fail = false
	f.off = true
	if err := m.Once(ctx); err != nil || len(f.sent) != 2 {
		t.Fatalf("mail off: %v %d", err, len(f.sent))
	}
	f.off = false
	if err := m.Once(ctx); err != nil || len(f.sent) != 3 || !strings.HasPrefix(f.sent[2], "openDECTcell: l3\n") {
		t.Fatalf("after the server came back: %v %q", err, f.sent)
	}
	// a notification that could not be mailed for a day is given up, not sent late
	n(loud, "l4", time.Date(2026, 11, 4, 0, 0, 0, 0, time.Local))
	if err := m.Once(ctx); err != nil || len(f.sent) != 3 {
		t.Fatalf("a stale one was sent: %v %d", err, len(f.sent))
	}
}

// If the end cannot be told (here: the table refuses the row), it is kept and told when it can be.
func TestEngineKeepsAnEndItCouldNotTell(t *testing.T) {
	pool := dbtest.Migrated(t)
	anna := user(t, pool, "anna", "user", `{}`)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	p := &probe{}
	e := &alerts.Engine{Pool: pool, Sources: []alerts.Source{p.source()}, Now: func() time.Time { return now }}
	p.set(alerts.Condition{Kind: alerts.KindLine, Object: "l1", Who: alerts.Who{Users: []uuid.UUID{anna}}, Level: "danger", Title: "down", OkTitle: "back"})
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	p.set(alerts.Condition{Status: alerts.Fine, Kind: alerts.KindLine, Object: "l1"})
	if _, err := pool.Exec(ctx, "ALTER TABLE notification ADD CONSTRAINT refuse CHECK (level <> 'ok')"); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err == nil {
		t.Fatal("the failure was not reported")
	}
	if _, err := pool.Exec(ctx, "ALTER TABLE notification DROP CONSTRAINT refuse"); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := notes(t, pool, anna); len(n) != 2 || n[1] != "ok:back" {
		t.Fatalf("the end was lost: %v", n)
	}
}
