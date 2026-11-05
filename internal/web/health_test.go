// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

type healthEnv struct {
	*env
	link     *onlineLink
	mail     *fakeMail
	statuses mapSIP
	dir      string
}

func newHealthEnv(t *testing.T, withMail bool) *healthEnv {
	h := &healthEnv{link: &onlineLink{up: map[uuid.UUID]bool{}, rfpi: map[uuid.UUID][5]byte{}}, mail: &fakeMail{}, statuses: mapSIP{}, dir: t.TempDir()}
	h.env = newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Cells, err = cell.Open(ctx, s.Pool, kr, "dect.example.org:7300"); err != nil {
			t.Fatal(err)
		}
		s.CellLink, s.SIP, s.Stats, s.BackupDir = h.link, h.statuses, &stats.Store{Pool: s.Pool}, h.dir
		if withMail {
			s.Mailer = h.mail
		}
		s.SMTPHost = "smtp.example.org"
	})
	return h
}

func TestHealthChecks(t *testing.T) {
	h := newHealthEnv(t, true)
	app := h.app
	want := func(name string, c healthCheck, status string, contains ...string) {
		t.Helper()
		if c.Status != status {
			t.Errorf("%s: status %s (%s), want %s", name, c.Status, c.Detail, status)
		}
		for _, w := range contains {
			if !strings.Contains(c.Detail, w) {
				t.Errorf("%s: %q lacks %q", name, c.Detail, w)
			}
		}
	}
	want("database", app.checkDatabase(ctx), healthOK, "ms")
	want("schema", app.checkSchema(ctx), healthOK, "migrations are applied")
	want("clock", app.checkClock(ctx), healthOK, "Agrees")
	want("mail", app.checkMail(ctx), healthOK, "smtp.example.org")

	// a database that is behind this program
	h.store.Pool.Exec(ctx, "DELETE FROM schema_migration WHERE version = (SELECT max(version) FROM schema_migration)")
	if c := app.checkSchema(ctx); c.Status == healthOK {
		t.Errorf("schema with a migration missing: %+v", c)
	}

	// the mail queue
	want("mail queue", app.checkMailQueue(ctx), healthOK)
	uid := uidOf(h.env, "user")
	h.store.Pool.Exec(ctx, "INSERT INTO notification (user_id, kind, level, subject, mail_wanted, at) VALUES ($1, 'line', 'danger', 's', true, now() - interval '3 hours')", uid)
	h.store.Pool.Exec(ctx, "INSERT INTO notification (user_id, kind, level, subject, mail_wanted, at) VALUES ($1, 'line', 'danger', 'new', true, now() - interval '5 minutes')", uid)
	want("mail queue", app.checkMailQueue(ctx), healthWarn, "1 notifications have waited")

	// base stations
	want("no stations", app.checkStations(ctx), healthInfo, "No base station")
	var a, b uuid.UUID
	h.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('A', 'active') RETURNING id").Scan(&a)
	h.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('B', 'active') RETURNING id").Scan(&b)
	h.store.Pool.Exec(ctx, "INSERT INTO cell (name, state) VALUES ('Off', 'disabled')")
	want("none online", app.checkStations(ctx), healthFail, "None of the 2")
	h.link.set(a, true)
	want("one online", app.checkStations(ctx), healthWarn, "1 of 2")
	h.link.set(b, true)
	want("all online", app.checkStations(ctx), healthOK, "All 2")

	// the authority
	h.store.Pool.Exec(ctx, "DELETE FROM cell_ca")
	want("no authority", app.checkCertificates(ctx), healthInfo)
	setCA := func(d time.Duration) {
		h.store.Pool.Exec(ctx, "DELETE FROM cell_ca")
		h.store.Pool.Exec(ctx, "INSERT INTO cell_ca (cert_der, key_enc, active, not_after) VALUES ('\\x00', '\\x', true, $1)", time.Now().Add(d))
	}
	day := 24 * time.Hour
	setCA(5 * 365 * day)
	want("a good authority", app.checkCertificates(ctx), healthOK, "more days")
	setCA(60 * day)
	want("an authority ending in 60 days", app.checkCertificates(ctx), healthWarn, "ends in 5")
	setCA(10 * day)
	want("an authority ending in 10 days", app.checkCertificates(ctx), healthFail)
	setCA(-day)
	want("an authority that ended", app.checkCertificates(ctx), healthFail, "has ended")

	// the lines
	want("no lines", app.checkLines(ctx), healthInfo)
	var l1, l2, l3 uuid.UUID
	for i, id := range []*uuid.UUID{&l1, &l2, &l3} {
		h.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain, enabled, register) VALUES ($1, $2, 'u', 'd.example', $3, true) RETURNING id",
			uid, "l"+string(rune('1'+i)), i != 2).Scan(id) // the third is switched off
	}
	h.statuses[l1] = sip.RegStatus{State: sip.RegRegistered}
	h.statuses[l2] = sip.RegStatus{State: sip.RegFailed}
	want("a line that fails", app.checkLines(ctx), healthWarn, "1 of 2 lines cannot register")
	h.statuses[l2] = sip.RegStatus{State: sip.RegRegistered}
	want("lines up", app.checkLines(ctx), healthOK, "2 of 2")

	// backups
	want("no state", app.checkBackup(ctx), healthWarn, "has not written a state")
	now := time.Now().UTC()
	os.WriteFile(filepath.Join(h.dir, "status.json"), []byte(statusJSON(true, now.Add(-time.Hour), "")), 0o600)
	want("a good backup", app.checkBackup(ctx), healthOK, "last good backup")
	os.WriteFile(filepath.Join(h.dir, "status.json"), []byte(statusJSON(false, now, "no space left")), 0o600)
	want("a failed backup", app.checkBackup(ctx), healthFail, "no space left")
	os.WriteFile(filepath.Join(h.dir, "status.json"), []byte("{broken"), 0o600)
	want("a broken state", app.checkBackup(ctx), healthWarn, "cannot be read")
	app.BackupDir = ""
	want("no directory", app.checkBackup(ctx), healthInfo)
	want("no directory for space", app.checkBackupSpace(ctx), healthInfo)
	app.BackupDir = h.dir

	// the space of the backup volume
	for _, c := range []struct {
		free, total uint64
		err         error
		status      string
	}{{50e9, 100e9, nil, healthOK}, {10e9, 100e9, nil, healthWarn}, {3e9, 100e9, nil, healthFail}, {0, 0, nil, healthInfo}, {0, 0, errors.New("x"), healthInfo}, {15e9, 100e9, nil, healthOK}, {14e9, 100e9, nil, healthWarn}, {5e9, 100e9, nil, healthWarn}, {4e9, 100e9, nil, healthFail}} {
		app.freeSpace = func(string) (uint64, uint64, error) { return c.free, c.total, c.err }
		if got := app.checkBackupSpace(ctx); got.Status != c.status {
			t.Errorf("free %d of %d: %s (%s), want %s", c.free, c.total, got.Status, got.Detail, c.status)
		}
	}
	// without a mail server
	app.Mailer = nil
	want("no mail", app.checkMail(ctx), healthWarn, "No mail server")
}

func TestHealthSummary(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{{nil, healthOK}, {[]string{healthOK, healthInfo}, healthOK}, {[]string{healthOK, healthWarn}, healthWarn}, {[]string{healthWarn, healthFail, healthOK}, healthFail}, {[]string{healthFail, healthWarn}, healthFail}} {
		var cs []healthCheck
		for _, s := range c.in {
			cs = append(cs, healthCheck{Status: s})
		}
		if got := healthSummary(cs); got != c.want {
			t.Errorf("%v: %s, want %s", c.in, got, c.want)
		}
	}
}

func TestHealthPageAndTestMail(t *testing.T) {
	h := newHealthEnv(t, true)
	a, u := h.adminBrowser(t), h.userBrowser(t)
	h.store.Pool.Exec(ctx, "UPDATE app_user SET email = 'admin@example.org' WHERE username = 'admin'")
	resp, body := h.get(t, a, "/admin/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	for _, want := range []string{"Database", "Database schema", "Clock", "Outgoing mail", "Base stations", "Certificates", "VoIP lines", "Backups", "Space for backups", "Send a test mail"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if _, nav := h.get(t, a, "/admin"); !strings.Contains(nav, `href="/admin/health"`) {
		t.Error("the menu has no Health entry")
	}
	for _, p := range []string{"/admin/health"} {
		if resp, body := h.get(t, u, p); resp.StatusCode == http.StatusOK || strings.Contains(body, "Database schema") {
			t.Errorf("a user got %s", p)
		}
	}
	if r := h.post(t, u, "/admin/health/mail", url.Values{}); r.StatusCode == http.StatusSeeOther && strings.Contains(r.Header.Get("Location"), "done") {
		t.Error("a user sent a test mail")
	}
	if resp, _ := h.get(t, h.browser(), "/admin/health"); resp.StatusCode == http.StatusOK {
		t.Error("the page without a login")
	}
	// the test mail goes to the administrator's own address
	r := h.post(t, a, "/admin/health/mail", url.Values{})
	if r.StatusCode != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "done=") {
		t.Fatalf("test mail: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if m := h.mail.wait(t, 1); m.to != "admin@example.org" || !strings.Contains(m.subject, "test") {
		t.Errorf("the test mail: %+v", m)
	}
	// no address, no mail server
	h.store.Pool.Exec(ctx, "UPDATE app_user SET email = '' WHERE username = 'admin'")
	if r := h.post(t, a, "/admin/health/mail", url.Values{}); !strings.Contains(r.Header.Get("Location"), "error=") || len(h.mail.sent) != 1 {
		t.Errorf("no address: %s", r.Header.Get("Location"))
	}
	h.app.Mailer = nil
	if r := h.post(t, a, "/admin/health/mail", url.Values{}); !strings.Contains(r.Header.Get("Location"), "error=") {
		t.Errorf("no mail server: %s", r.Header.Get("Location"))
	}
}

func TestPublicStatus(t *testing.T) {
	h := newHealthEnv(t, true)
	anon := h.browser()
	if resp, _ := h.get(t, anon, "/status"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/status when it is off: %d", resp.StatusCode)
	}
	h.app.PublicStatus = true
	resp, body := h.get(t, anon, "/status")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"status":"ok"`) || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("/status: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "Database") || strings.Contains(body, "smtp") || strings.Contains(body, "migrations") {
		t.Errorf("the public status shows what is inside: %s", body)
	}
	// a failing check makes it degraded: two active base stations, none connected
	h.store.Pool.Exec(ctx, "INSERT INTO cell (name, state) VALUES ('A', 'active')")
	if _, body = h.get(t, anon, "/status"); !strings.Contains(body, `"status":"degraded"`) {
		t.Errorf("with every base station offline: %s", body)
	}
}

func TestHealthJudgements(t *testing.T) {
	ms := time.Millisecond
	for _, c := range []struct {
		d    time.Duration
		want string
	}{{20 * ms, healthOK}, {500 * ms, healthOK}, {501 * ms, healthWarn}, {3000 * ms, healthWarn}} {
		if got, _ := databaseHealth(c.d); got != c.want {
			t.Errorf("database %v: %s", c.d, got)
		}
	}
	for _, c := range []struct {
		skew time.Duration
		want string
	}{{0, healthOK}, {2 * time.Second, healthOK}, {2100 * ms, healthWarn}, {-5 * time.Second, healthWarn}, {30 * time.Second, healthWarn}, {31 * time.Second, healthFail}, {-90 * time.Second, healthFail}} {
		if got, detail := clockHealth(c.skew, "Europe/Berlin"); got != c.want || (c.want != healthOK && !strings.Contains(detail, "differ")) {
			t.Errorf("clock %v: %s (%s)", c.skew, got, detail)
		}
	}
}

// Base station certificates that could not renew, or a line that waits to retry, are what the page says they are.
func TestHealthCertificatesAndLinesInTheirOtherStates(t *testing.T) {
	h := newHealthEnv(t, true)
	// a good authority, and an active base station whose only certificate ends in 5 days and has no successor
	h.store.Pool.Exec(ctx, "DELETE FROM cell_ca")
	h.store.Pool.Exec(ctx, "INSERT INTO cell_ca (cert_der, key_enc, active, not_after) VALUES ('\\x00', '\\x', true, now() + interval '3000 days')")
	var cid uuid.UUID
	h.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('Attic', 'active') RETURNING id").Scan(&cid)
	h.store.Pool.Exec(ctx, "INSERT INTO cell_certificate (fingerprint, cell_id, serial, not_before, not_after) VALUES (decode(repeat('ab', 32), 'hex'), $1, '1', now() - interval '80 days', now() + interval '5 days')", cid)
	if c := h.app.checkCertificates(ctx); c.Status != healthFail || !strings.Contains(c.Detail, "1 base station certificates") {
		t.Errorf("a certificate that could not renew: %+v", c)
	}
	h.store.Pool.Exec(ctx, "INSERT INTO cell_certificate (fingerprint, cell_id, serial, not_before, not_after) VALUES (decode(repeat('cd', 32), 'hex'), $1, '2', now(), now() + interval '90 days')", cid)
	if c := h.app.checkCertificates(ctx); c.Status != healthOK {
		t.Errorf("after it renewed: %+v", c)
	}
	// a line that waits to try again is not registered either
	uid := uidOf(h.env, "user")
	var line uuid.UUID
	h.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'l', 'u', 'd.example') RETURNING id", uid).Scan(&line)
	h.statuses[line] = sip.RegStatus{State: sip.RegBackoff, LastError: "timeout"}
	if c := h.app.checkLines(ctx); c.Status != healthWarn {
		t.Errorf("a line in backoff: %+v", c)
	}
}
