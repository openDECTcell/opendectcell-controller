// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/alerts"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

func uidOf(e *env, name string) uuid.UUID {
	var id uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = $1", name).Scan(&id)
	return id
}

func TestNotificationsPage(t *testing.T) {
	e := newEnv(t)
	u := e.userBrowser(t)
	me, other := uidOf(e, "user"), uidOf(e, "admin")
	if resp, body := e.get(t, u, "/notifications"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Nothing to tell") {
		t.Fatalf("empty page: %d", resp.StatusCode)
	}
	add := func(user uuid.UUID, level, subject, href string) {
		e.store.Pool.Exec(ctx, "INSERT INTO notification (user_id, kind, level, subject, body, href) VALUES ($1, 'line', $2, $3, 'the details', $4)", user, level, subject, href)
	}
	add(me, "danger", "Line Home is down", "/sip-accounts/x")
	add(me, "ok", "Line Home is back", "")
	add(other, "danger", "Somebody else's secret line", "")
	resp, body := e.get(t, u, "/notifications")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Line Home is down") || !strings.Contains(body, "Line Home is back") || !strings.Contains(body, "the details") {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	if strings.Contains(body, "Somebody else") {
		t.Error("another user's notification is shown")
	}
	// the bell on every page says how many are unread
	if _, home := e.get(t, u, "/account"); !strings.Contains(home, "Notifications, 2 unread") {
		t.Error("the unread count is not on the pages")
	}
	// mark one read: the count drops; another user's cannot be marked
	var id, theirs int64
	e.store.Pool.QueryRow(ctx, "SELECT id FROM notification WHERE subject = 'Line Home is down'").Scan(&id)
	e.store.Pool.QueryRow(ctx, "SELECT id FROM notification WHERE user_id = $1", other).Scan(&theirs)
	e.post(t, u, "/notifications/read", url.Values{"id": {itoa64(theirs)}})
	var n int
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM notification WHERE read_at IS NOT NULL AND user_id = $1", other).Scan(&n)
	if n != 0 {
		t.Fatal("marked another user's notification as read")
	}
	e.post(t, u, "/notifications/read", url.Values{"id": {itoa64(id)}})
	if _, home := e.get(t, u, "/account"); !strings.Contains(home, "Notifications, 1 unread") {
		t.Error("the count did not drop")
	}
	e.post(t, u, "/notifications/read", url.Values{})
	if _, home := e.get(t, u, "/account"); strings.Contains(home, "unread") {
		t.Error("mark all read left some")
	}
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM notification WHERE read_at IS NULL AND user_id = $1", other).Scan(&n)
	if n != 1 {
		t.Error("mark all read touched another user's")
	}
	if r := e.post(t, e.browser(), "/notifications/read", url.Values{}); r.StatusCode == http.StatusSeeOther && strings.Contains(r.Header.Get("Location"), "/notifications") {
		t.Error("marking read without a login")
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func TestAlertPreferencesForm(t *testing.T) {
	e := newEnv(t)
	u := e.userBrowser(t)
	e.store.Pool.Exec(ctx, "UPDATE app_user SET email = 'user@example.org' WHERE username = 'user'")
	prefs := func(username string) alerts.Prefs {
		var raw []byte
		e.store.Pool.QueryRow(ctx, "SELECT alert_prefs FROM app_user WHERE username = $1", username).Scan(&raw)
		return alerts.ParsePrefs(raw)
	}
	// the page shows the sections the user may have; a user has no certificate kind
	_, body := e.get(t, u, "/account")
	if !strings.Contains(body, "A VoIP line cannot register") || !strings.Contains(body, `name="show_line" checked`) || strings.Contains(body, "certificate authority") {
		t.Fatalf("the form: %s", body)
	}
	// hide one, mail one, a handset time, quiet hours
	r := e.post(t, u, "/account/alerts", url.Values{"show_handset": {"on"}, "show_cell": {"on"}, "mail_line": {"on"}, "mail_cert": {"on"},
		"handset_hours": {"48"}, "quiet_from": {"22:00"}, "quiet_to": {"07:00"}})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d %s", r.StatusCode, readBody(r))
	}
	p := prefs("user")
	if p.Shows(alerts.KindLine) || !p.Mails(alerts.KindLine) || !p.Shows(alerts.KindHandset) || p.HandsetHours != 48 || p.QuietFrom != "22:00" || p.QuietTo != "07:00" {
		t.Errorf("stored: %+v", p)
	}
	if p.Mails(alerts.KindCert) {
		t.Error("a user set the administrators' kind")
	}
	// the mail for a base station is the older option's: a forged field does not make a second way
	e.post(t, u, "/account/alerts", url.Values{"show_line": {"on"}, "mail_cell": {"on"}})
	if prefs("user").Mails(alerts.KindCell) {
		t.Error("mail for base stations was set on this form")
	}
	e.post(t, u, "/account/alerts", url.Values{"show_handset": {"on"}, "show_cell": {"on"}, "mail_line": {"on"}, "mail_cert": {"on"},
		"handset_hours": {"48"}, "quiet_from": {"22:00"}, "quiet_to": {"07:00"}})
	// the form shows what was stored
	_, body = e.get(t, u, "/account")
	for _, want := range []string{`name="mail_line" checked`, `value="48"`, `value="22:00"`, `value="07:00"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the form does not show %s", want)
		}
	}
	if strings.Contains(body, `name="show_line" checked`) {
		t.Error("the hidden kind is shown as ticked")
	}
	for name, v := range map[string]url.Values{
		"bad time":       {"show_line": {"on"}, "quiet_from": {"25:00"}, "quiet_to": {"07:00"}},
		"one end":        {"show_line": {"on"}, "quiet_from": {"22:00"}},
		"hours too many": {"show_line": {"on"}, "handset_hours": {"100000"}},
		"hours text":     {"show_line": {"on"}, "handset_hours": {"many"}},
	} {
		if r := e.post(t, u, "/account/alerts", v); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", name, r.StatusCode)
		}
	}
	if p := prefs("user"); p.QuietFrom != "22:00" || p.HandsetHours != 48 {
		t.Errorf("a refused form changed the choices: %+v", p)
	}
	// mail needs an address
	e.store.Pool.Exec(ctx, "UPDATE app_user SET email = '' WHERE username = 'user'")
	if r := e.post(t, u, "/account/alerts", url.Values{"show_line": {"on"}, "mail_line": {"on"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("mail without an address: %d", r.StatusCode)
	}
	// an administrator may choose the certificate kind
	a := e.adminBrowser(t)
	e.store.Pool.Exec(ctx, "UPDATE app_user SET email = 'admin@example.org' WHERE username = 'admin'")
	e.post(t, a, "/account/alerts", url.Values{"show_cert": {"on"}, "mail_cert": {"on"}})
	if p := prefs("admin"); !p.Mails(alerts.KindCert) || !p.Shows(alerts.KindCert) {
		t.Errorf("admin: %+v", p)
	}
}

type mapSIP map[uuid.UUID]sip.RegStatus

func (m mapSIP) Status(id uuid.UUID) (sip.RegStatus, bool) { st, ok := m[id]; return st, ok }

func TestAlertSources(t *testing.T) {
	statuses := mapSIP{}
	e := newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.SIPAccounts, s.SIP = &sipaccount.Store{Pool: s.Pool, Keyring: kr}, statuses
		s.Stats = &stats.Store{Pool: s.Pool}
	})
	now := time.Now()
	me, adm := uidOf(e, "user"), uidOf(e, "admin")
	line := func(owner any, label string, enabled, register bool) uuid.UUID {
		var id uuid.UUID
		if err := e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain, enabled, register) VALUES ($1, $2, $2, 'd.example', $3, $4) RETURNING id", owner, label, enabled, register).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	down, up, trying, shared, off, noReg, unknown := line(me, "Down", true, true), line(me, "Up", true, true), line(me, "Trying", true, true), line(nil, "Shared", true, true),
		line(me, "Off", false, true), line(me, "NoReg", true, false), line(me, "Unknown", true, true)
	statuses[down] = sip.RegStatus{State: sip.RegFailed, LastError: "403 Forbidden"}
	statuses[up] = sip.RegStatus{State: sip.RegRegistered}
	statuses[trying] = sip.RegStatus{State: sip.RegRegistering}
	statuses[shared] = sip.RegStatus{State: sip.RegBackoff, LastError: "timeout"}
	statuses[off] = sip.RegStatus{State: sip.RegFailed}
	statuses[noReg] = sip.RegStatus{State: sip.RegFailed}
	got := map[string]alerts.Condition{}
	conds, err := e.app.lineConditions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conds {
		got[c.Object] = c
	}
	if c := got[down.String()]; c.Status != alerts.Wrong || c.After != LineDownAfter || len(c.Who.Users) != 1 || c.Who.Users[0] != me || c.Who.Admins ||
		!strings.Contains(c.Title, "Down") || !strings.Contains(c.Body, "403 Forbidden") || c.Href != "/sip-accounts/"+down.String() || c.OkTitle == "" {
		t.Errorf("a failing line: %+v", c)
	}
	if c := got[shared.String()]; c.Status != alerts.Wrong || !c.Who.Admins || len(c.Who.Users) != 0 {
		t.Errorf("a shared line is the administrators': %+v", c)
	}
	if got[up.String()].Status != alerts.Fine || got[trying.String()].Status != alerts.Unsure || got[unknown.String()].Status != alerts.Unsure {
		t.Errorf("fine and in between: %+v %+v %+v", got[up.String()], got[trying.String()], got[unknown.String()])
	}
	if _, ok := got[off.String()]; ok {
		t.Error("a line that is switched off is watched")
	}
	if _, ok := got[noReg.String()]; ok {
		t.Error("a line that does not register is watched")
	}

	// handsets: only for a user who asked, for as long as they asked
	handset := func(owner uuid.UUID, name string, seen time.Duration) uuid.UUID {
		var id uuid.UUID
		if err := e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name, last_seen_at) VALUES ($1, $2, $3, '\\x', $4, $5) RETURNING id",
			owner, []byte(name), int(name[0]), name, now.Add(-seen)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	quiet, fresh, theirs := handset(me, "Quiet", 72*time.Hour), handset(me, "Fresh", 2*time.Hour), handset(adm, "Theirs", 500*time.Hour)
	if conds, err = e.app.handsetConditions(ctx, now); err != nil || len(conds) != 0 {
		t.Fatalf("nobody asked: %v %+v", err, conds)
	}
	e.store.Pool.Exec(ctx, "UPDATE app_user SET alert_prefs = $1 WHERE id = $2", alerts.Prefs{HandsetHours: 48}.JSON(), me)
	conds, err = e.app.handsetConditions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]alerts.Condition{}
	for _, c := range conds {
		got[c.Object] = c
	}
	if c := got[quiet.String()]; c.Status != alerts.Wrong || c.After != 48*time.Hour || len(c.Who.Users) != 1 || c.Who.Users[0] != me || !strings.Contains(c.Title, "Quiet") || c.Since.IsZero() {
		t.Errorf("a handset not heard for 72 h with 48 asked: %+v", c)
	}
	if got[fresh.String()].Status != alerts.Fine {
		t.Errorf("a handset heard 2 h ago: %+v", got[fresh.String()])
	}
	if _, ok := got[theirs.String()]; ok {
		t.Error("a handset of a user who did not ask is watched")
	}

	// certificates: the authority of the base stations
	check := func(end time.Time) map[string]alerts.Condition {
		e.store.Pool.Exec(ctx, "DELETE FROM cell_ca")
		e.store.Pool.Exec(ctx, "INSERT INTO cell_ca (cert_der, key_enc, active, not_after) VALUES ('\\x00', '\\x', true, $1)", end)
		cs, err := e.app.certConditions(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]alerts.Condition{}
		for _, c := range cs {
			m[c.Object] = c
		}
		return m
	}
	if c := check(now.AddDate(0, 0, 30))["ca"]; c.Status != alerts.Wrong || !c.Who.Admins || !strings.Contains(c.Title, "ends on") {
		t.Errorf("an authority that ends in 30 days: %+v", c)
	}
	if c := check(now.AddDate(5, 0, 0)); c["ca"].Status != alerts.Fine || c["cell-certificates"].Status != alerts.Fine {
		t.Errorf("an authority that ends in 5 years: %+v", c)
	}
}

// A base station that goes offline, and comes back, is in the notification centre of its operator and of the administrators.
func TestCellOfflineIsInTheNotificationCentre(t *testing.T) {
	e, link, _ := newOpsEnv(t)
	owner := uidOf(e, "user")
	var cid uuid.UUID
	if err := e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state, enrolled_by) VALUES ('Attic', 'active', $1) RETURNING id", owner).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	state := map[uuid.UUID]*cellAlert{}
	now := time.Now()
	e.app.checkCellAlerts(ctx, now, state)
	e.app.checkCellAlerts(ctx, now.Add(CellOfflineAfter+time.Minute), state)
	link.set(cid, true)
	e.app.checkCellAlerts(ctx, now.Add(CellOfflineAfter+2*time.Minute), state)
	for _, who := range []string{"user", "admin"} {
		rows, _ := e.store.Pool.Query(ctx, "SELECT level, subject, href FROM notification WHERE user_id = $1 AND kind = 'cell' ORDER BY id", uidOf(e, who))
		var got []string
		for rows.Next() {
			var l, s, h string
			rows.Scan(&l, &s, &h)
			got = append(got, l+"|"+s+"|"+h)
		}
		rows.Close()
		if len(got) != 2 || !strings.HasPrefix(got[0], "danger|Cell offline: Attic|/stations/") || !strings.HasPrefix(got[1], "ok|Cell back online: Attic|") {
			t.Errorf("%s: %v", who, got)
		}
	}
}
