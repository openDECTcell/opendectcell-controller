// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/alerts"
	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
	"github.com/openDECTcell/opendectcell-controller/internal/netguard"
	"github.com/openDECTcell/opendectcell-controller/internal/webhook"
)

func newHookEnv(t *testing.T, allowPrivate bool) *env {
	return newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.Contacts = &contacts.Store{Pool: s.Pool}
		s.Handsets = &handset.Store{Pool: s.Pool}
		s.Webhooks = &webhook.Store{Pool: s.Pool, Keyring: kr, Policy: netguard.Policy{AllowPrivate: allowPrivate}}
	})
}

var whsecRE = regexp.MustCompile(`whsec_[A-Za-z0-9_-]{43}`)

func hookForm(name, address, password string, events ...string) url.Values {
	v := url.Values{"name": {name}, "url": {address}, "password": {password}, "enabled": {"on"}, "format": {"json"}}
	for _, e := range events {
		v.Set("event_"+e, "on")
	}
	return v
}

func TestWebhookPages(t *testing.T) {
	e := newHookEnv(t, false)
	u := e.userBrowser(t)
	if resp, body := e.get(t, u, "/account/webhooks"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Add a webhook") || !strings.Contains(body, "No webhooks yet") {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	if _, body := e.get(t, u, "/account"); !strings.Contains(body, `href="/account/webhooks"`) {
		t.Error("no link from the account page")
	}
	// refused, each for its reason, and nothing is made
	for name, c := range map[string]struct {
		form url.Values
		want string
	}{
		"wrong password": {hookForm("x", "https://example.org/h", "nope", "call.missed"), "The password is wrong"},
		"http":           {hookForm("x", "http://example.org/h", "user password", "call.missed"), "https://"},
		"metadata":       {hookForm("x", "https://169.254.169.254/latest", "user password", "call.missed"), "does not post to"},
		"loopback":       {hookForm("x", "https://127.0.0.1/h", "user password", "call.missed"), "does not post to"},
		"private":        {hookForm("x", "https://192.168.1.5/h", "user password", "call.missed"), "does not post to"},
		"credentials":    {hookForm("x", "https://u:p@example.org/h", "user password", "call.missed"), "user name or password"},
		"no event":       {hookForm("x", "https://example.org/h", "user password"), "at least one event"},
		"no name":        {hookForm(" ", "https://example.org/h", "user password", "call.missed"), "name"},
	} {
		r := e.post(t, u, "/account/webhooks", c.form)
		if body := readBody(r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %.200s", name, r.StatusCode, body)
		}
	}
	var n int
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook").Scan(&n)
	if n != 0 {
		t.Fatalf("%d webhooks after refusals", n)
	}
	// made: the secret is shown once
	form := hookForm("Home Assistant", "https://ha.example.org/api/webhook/abc-secret-token", "user password", "call.incoming", "call.missed")
	form.Set("auth", "Bearer tk_1234")
	r := e.post(t, u, "/account/webhooks", form)
	body := readBody(r)
	sec := whsecRE.FindString(body)
	if sec == "" || r.StatusCode != http.StatusOK || !strings.Contains(body, "only this once") || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %.200s", r.StatusCode, body)
	}
	if strings.Contains(body, "tk_1234") {
		t.Error("the Authorization value is on the page")
	}
	var id uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM webhook").Scan(&id)
	loc := "/account/webhooks/" + id.String()
	for _, p := range []string{"/account/webhooks", loc, "/account", "/account/export"} {
		if _, page := e.get(t, u, p); strings.Contains(page, sec) || strings.Contains(page, "tk_1234") {
			t.Errorf("a secret is on %s", p)
		}
	}
	// the list shows the host, not the path (the path may be a secret)
	_, list := e.get(t, u, "/account/webhooks")
	if !strings.Contains(list, "ha.example.org") || strings.Contains(list, "abc-secret-token") || !strings.Contains(list, "call.missed") {
		t.Errorf("list: %s", list)
	}
	// the page of the webhook shows the address, for editing
	_, page := e.get(t, u, loc)
	if !strings.Contains(page, "abc-secret-token") || !strings.Contains(page, "Nothing has been sent to it yet") || !strings.Contains(page, "one is stored") {
		t.Errorf("webhook page: %s", page)
	}
	// the export of the user's data lists it without the address's path and without secrets
	if _, ex := e.get(t, u, "/account/export"); !strings.Contains(ex, `"webhooks"`) || !strings.Contains(ex, "Home Assistant") || strings.Contains(ex, "abc-secret-token") {
		t.Errorf("export: %.400s", ex)
	}

	// update: the password again, the rules again
	up := hookForm("HA", "https://ha.example.org/api/webhook/other", "user password", "call.ended")
	if r := e.post(t, u, loc, hookForm("HA", "https://ha.example.org/x", "wrong", "call.ended")); r.StatusCode != http.StatusBadRequest {
		t.Errorf("update with a wrong password: %d", r.StatusCode)
	}
	if r := e.post(t, u, loc, hookForm("HA", "http://ha.example.org/x", "user password", "call.ended")); r.StatusCode != http.StatusBadRequest {
		t.Errorf("update to http: %d", r.StatusCode)
	}
	if r := e.post(t, u, loc, up); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: %d", r.StatusCode)
	}
	var name, events string
	var hasAuth bool
	e.store.Pool.QueryRow(ctx, "SELECT name, array_to_string(events, ','), auth_enc IS NOT NULL FROM webhook WHERE id = $1", id).Scan(&name, &events, &hasAuth)
	if name != "HA" || events != "call.ended" || !hasAuth {
		t.Errorf("after the update: %q %q auth %v (the Authorization value stays unless told)", name, events, hasAuth)
	}
	// rotate: a new secret, once; needs the password
	if r := e.post(t, u, loc+"/rotate", url.Values{"password": {"wrong"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("rotate with a wrong password: %d", r.StatusCode)
	}
	r = e.post(t, u, loc+"/rotate", url.Values{"password": {"user password"}})
	body = readBody(r)
	if sec2 := whsecRE.FindString(body); sec2 == "" || sec2 == sec || r.StatusCode != http.StatusOK {
		t.Errorf("rotate: %d %.100s", r.StatusCode, body)
	}
	// a test event is queued for it
	if r := e.post(t, u, loc+"/test", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("test: %d", r.StatusCode)
	}
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_delivery WHERE webhook_id = $1 AND event = 'ping'", id).Scan(&n)
	if n != 1 {
		t.Errorf("%d test events queued", n)
	}
	if _, page = e.get(t, u, loc); !strings.Contains(page, "ping") || !strings.Contains(page, "waiting") {
		t.Error("the queued test is not in the log")
	}

	// another user finds none of it
	a := e.adminBrowser(t)
	for _, c := range []struct{ method, path string }{{"GET", loc}, {"POST", loc}, {"POST", loc + "/rotate"}, {"POST", loc + "/test"}, {"POST", loc + "/delete"}} {
		var resp *http.Response
		if c.method == "GET" {
			resp, _ = e.get(t, a, c.path)
		} else {
			resp = e.post(t, a, c.path, hookForm("x", "https://example.org/x", "admin password", "call.ended"))
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s by another user: %d", c.method, c.path, resp.StatusCode)
		}
	}
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook").Scan(&n)
	if n != 1 {
		t.Error("another user deleted it")
	}
	if _, la := e.get(t, a, "/account/webhooks"); strings.Contains(la, "ha.example.org") {
		t.Error("another user sees it listed")
	}
	// delete
	if r := e.post(t, u, loc+"/delete", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook").Scan(&n)
	if n != 0 {
		t.Error("not deleted")
	}
	if resp, _ := e.get(t, e.browser(), "/account/webhooks"); resp.StatusCode == http.StatusOK {
		t.Error("the page without a login")
	}
}

func TestPlainHTTPWhereThePrivateNetworkIsAllowed(t *testing.T) {
	e := newHookEnv(t, true)
	u := e.userBrowser(t)
	if r := e.post(t, u, "/account/webhooks", hookForm("Home", "http://192.168.1.20:8123/api/webhook/x", "user password", "call.missed")); r.StatusCode != http.StatusOK {
		t.Errorf("http on the home network: %d", r.StatusCode)
	}
	if r := e.post(t, u, "/account/webhooks", hookForm("Home", "http://127.0.0.1:8123/x", "user password", "call.missed")); r.StatusCode != http.StatusBadRequest {
		t.Errorf("loopback is never allowed: %d", r.StatusCode)
	}
	if _, body := e.get(t, u, "/account/webhooks"); !strings.Contains(body, "on your own network") {
		t.Error("the page does not say that http is allowed here")
	}
}

// waitFor polls until cond holds: the events are queued off the call's path.
func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCallsBecomeWebhookEvents(t *testing.T) {
	e := newHookEnv(t, false)
	uid, other := uidOf(e, "user"), uidOf(e, "admin")
	var hs, line uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x0a', 10, '\\x', 'Kitchen') RETURNING id", uid).Scan(&hs)
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Home', 'u', 'd.example') RETURNING id", uid).Scan(&line)
	e.store.Pool.Exec(ctx, "INSERT INTO contact (owner_user_id, last_name, first_name) VALUES ($1, 'Meier', 'Eva')", uid)
	e.store.Pool.Exec(ctx, "INSERT INTO contact_number (contact_id, number, kind, is_default, position) SELECT id, '+4930123456', 'work', true, 0 FROM contact WHERE last_name = 'Meier'")
	if _, _, err := e.app.Webhooks.Create(ctx, uid, webhook.Input{Name: "all", URL: "https://example.org/a", Events: []string{"call.incoming", "call.missed", "call.ended"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.app.Webhooks.Create(ctx, other, webhook.Input{Name: "theirs", URL: "https://example.org/b", Events: []string{"call.incoming", "call.missed", "call.ended"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	log := &calllog.Store{Pool: e.store.Pool, Hook: e.app.CallEvent}
	events := func() map[string]int {
		rows, _ := e.store.Pool.Query(ctx, "SELECT d.event, count(*) FROM webhook_delivery d JOIN webhook w ON w.id = d.webhook_id WHERE w.user_id = $1 GROUP BY 1", uid)
		defer rows.Close()
		m := map[string]int{}
		for rows.Next() {
			var ev string
			var n int
			rows.Scan(&ev, &n)
			m[ev] = n
		}
		return m
	}
	now := time.Now()
	// an incoming call that nobody answers: incoming, then ended and missed
	id, err := log.Start(ctx, calllog.Call{HandsetID: &hs, SIPAccountID: &line, Direction: "in", RemoteParty: "+4930123456", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "call.incoming", func() bool { return events()["call.incoming"] == 1 })
	if err := log.Ended(ctx, id, calllog.End{At: now.Add(20 * time.Second), Reason: "no_answer"}); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "call.missed", func() bool { return events()["call.missed"] == 1 && events()["call.ended"] == 1 })
	var payload string
	e.store.Pool.QueryRow(ctx, "SELECT payload::text FROM webhook_delivery d JOIN webhook w ON w.id = d.webhook_id WHERE w.user_id = $1 AND d.event = 'call.missed'", uid).Scan(&payload)
	for _, want := range []string{`"type": "call.missed"`, `"remote_party": "+4930123456"`, `"remote_name": "Eva Meier"`, `"handset": "Kitchen"`, `"line": "Home"`, `"result": "missed"`, "Missed call from Eva Meier (+4930123456)"} {
		if !strings.Contains(strings.ReplaceAll(payload, `":"`, `": "`), want) && !strings.Contains(payload, strings.ReplaceAll(want, `": "`, `":"`)) {
			t.Errorf("the missed-call event lacks %q: %s", want, payload)
		}
	}
	// an answered incoming call: incoming and ended, and no missed
	id2, _ := log.Start(ctx, calllog.Call{HandsetID: &hs, Direction: "in", RemoteParty: "+49999", StartedAt: now})
	log.Answered(ctx, id2, now.Add(2*time.Second))
	log.Ended(ctx, id2, calllog.End{At: now.Add(60 * time.Second), Reason: "normal"})
	waitForCond(t, "the second call", func() bool { return events()["call.incoming"] == 2 && events()["call.ended"] == 2 })
	// an outgoing call: only its end
	id3, _ := log.Start(ctx, calllog.Call{HandsetID: &hs, Direction: "out", RemoteParty: "0301", StartedAt: now})
	log.Ended(ctx, id3, calllog.End{At: now.Add(5 * time.Second), Reason: "busy"})
	waitForCond(t, "the third call", func() bool { return events()["call.ended"] == 3 })
	time.Sleep(200 * time.Millisecond)
	got := events()
	if got["call.incoming"] != 2 || got["call.missed"] != 1 || got["call.ended"] != 3 {
		t.Errorf("events: %v (want incoming 2, missed 1, ended 3: an outgoing call is not incoming, an answered one is not missed)", got)
	}
	// the other user's webhook heard of none of it
	var n int
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_delivery d JOIN webhook w ON w.id = d.webhook_id WHERE w.user_id = $1", other).Scan(&n)
	if n != 0 {
		t.Errorf("another user's webhook got %d events", n)
	}
	// a call of nobody's (no owner) is nobody's event
	e.store.Pool.Exec(ctx, "UPDATE webhook_delivery SET state = 'done'")
	idNone, _ := log.Start(ctx, calllog.Call{Direction: "in", RemoteParty: "1", StartedAt: now})
	_ = idNone
	time.Sleep(200 * time.Millisecond)
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_delivery WHERE state = 'pending'").Scan(&n)
	if n != 0 {
		t.Errorf("a call without an owner made %d events", n)
	}
}

func TestNotificationsBecomeWebhookEvents(t *testing.T) {
	e := newHookEnv(t, false)
	uid := uidOf(e, "user")
	e.app.Webhooks.Create(ctx, uid, webhook.Input{Name: "n", URL: "https://example.org/n", Events: []string{"notification"}, Enabled: true})
	err := alerts.Notify(ctx, e.store.Pool, alerts.Who{Users: []uuid.UUID{uid}}, alerts.Notification{Kind: alerts.KindLine, Level: "danger", Subject: "The VoIP line Home cannot register", Body: "403", Href: "/sip-accounts/x"}, e.app.NotificationEvent)
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	waitForCond(t, "the notification event", func() bool {
		return e.store.Pool.QueryRow(ctx, "SELECT payload::text FROM webhook_delivery WHERE event = 'notification'").Scan(&payload) == nil
	})
	if !strings.Contains(payload, `"summary": "The VoIP line Home cannot register"`) {
		t.Errorf("the event has no summary sentence (the ntfy format reads it): %s", payload)
	}
	for _, want := range []string{"The VoIP line Home cannot register", `"kind"`, "danger", "/sip-accounts/x"} {
		if !strings.Contains(payload, want) {
			t.Errorf("the notification event lacks %q: %s", want, payload)
		}
	}
}

func TestHandsetPresenceBecomesWebhookEvents(t *testing.T) {
	e := newHookEnv(t, false)
	uid := uidOf(e, "user")
	var hs uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name, last_seen_at) VALUES ($1, '\\x0a', 10, '\\x', 'Kitchen', now()) RETURNING id", uid).Scan(&hs)
	if _, _, err := e.app.Webhooks.Create(ctx, uid, webhook.Input{Name: "p", URL: "https://example.org/p", Events: []string{"handset.online", "handset.offline"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// a user with no such webhook is not looked at
	prev := map[uuid.UUID]bool{}
	count := func(ev string) int {
		var n int
		e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_delivery WHERE event = $1", ev).Scan(&n)
		return n
	}
	e.app.checkHandsetWebhooks(ctx, prev) // the first look: online, no event
	time.Sleep(150 * time.Millisecond)
	if count("handset.online")+count("handset.offline") != 0 || !prev[hs] {
		t.Fatalf("the first look made an event, or did not note the handset: %v", prev)
	}
	e.store.Pool.Exec(ctx, "UPDATE handset SET detached_at = now() + interval '1 second' WHERE id = $1", hs) // it said it was switched off
	e.app.checkHandsetWebhooks(ctx, prev)
	waitForCond(t, "handset.offline", func() bool { return count("handset.offline") == 1 })
	e.app.checkHandsetWebhooks(ctx, prev) // nothing changed: nothing said
	time.Sleep(150 * time.Millisecond)
	if count("handset.offline") != 1 {
		t.Errorf("the same change was told twice")
	}
	e.store.Pool.Exec(ctx, "UPDATE handset SET detached_at = NULL, last_seen_at = now() WHERE id = $1", hs)
	e.app.checkHandsetWebhooks(ctx, prev)
	waitForCond(t, "handset.online", func() bool { return count("handset.online") == 1 })
	var payload string
	e.store.Pool.QueryRow(ctx, "SELECT payload::text FROM webhook_delivery WHERE event = 'handset.online'").Scan(&payload)
	if !strings.Contains(payload, "The handset Kitchen is heard again") {
		t.Errorf("payload: %s", payload)
	}
	// a handset that is removed is forgotten
	e.store.Pool.Exec(ctx, "DELETE FROM handset WHERE id = $1", hs)
	e.app.checkHandsetWebhooks(ctx, prev)
	if len(prev) != 0 {
		t.Errorf("a removed handset is still watched: %v", prev)
	}
}
