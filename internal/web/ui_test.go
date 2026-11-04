// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
)

func (e *env) adminBrowser(t *testing.T) *http.Client {
	c := e.browser()
	if r := e.login(t, c, "admin", "admin password"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin login: %d", r.StatusCode)
	}
	return c
}

func (e *env) userBrowser(t *testing.T) *http.Client {
	c := e.browser()
	if r := e.login(t, c, "user", "user password"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("user login: %d", r.StatusCode)
	}
	return c
}

// The menu shows the admin group to admins only, marks the current page,
// and loads nothing from outside.
func TestNavigationAndNoExternalRequests(t *testing.T) {
	e := newEnv(t)
	_, adminHome := e.get(t, e.adminBrowser(t), "/")
	_, userHome := e.get(t, e.userBrowser(t), "/")
	for _, want := range []string{"Administration", "/admin/settings", "/admin/logging", "/cells", "Public map"} {
		if !strings.Contains(adminHome, want) {
			t.Errorf("admin menu lacks %q", want)
		}
	}
	for _, leak := range []string{"Administration", "/admin/settings", "/cells", "/admin/audit"} {
		if strings.Contains(userHome, leak) {
			t.Errorf("user menu shows %q", leak)
		}
	}
	if !strings.Contains(userHome, `href="/statistics"`) || !strings.Contains(userHome, `href="/logging"`) {
		t.Error("user menu lacks statistics or logging")
	}
	if !strings.Contains(adminHome, `aria-current="page"`) {
		t.Error("no current page marked")
	}
	for _, page := range []string{adminHome, userHome} {
		for _, bad := range []string{"http://", "https://cdn", "https://fonts", "<style", " style=", "onclick="} {
			// SVG namespaces and our own source link are not requests
			clean := strings.NewReplacer(`xmlns="http://www.w3.org/2000/svg"`, "", "https://github.com/openDECTcell/opendectcell-controller", "").Replace(page)
			if strings.Contains(clean, bad) {
				t.Errorf("page contains %q", bad)
			}
		}
	}
}

func TestStubPages(t *testing.T) {
	e := newEnv(t)
	u, a := e.userBrowser(t), e.adminBrowser(t)

	if resp, body := e.get(t, u, "/calls"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Not built yet") {
		t.Errorf("/calls: %d", resp.StatusCode)
	}
	_ = a
}

func TestSelfRegistration(t *testing.T) {
	e := newEnv(t)
	c := e.browser()
	if resp, _ := e.get(t, c, "/register"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("registration should be off by default: %d", resp.StatusCode)
	}
	if _, body := e.get(t, c, "/login"); strings.Contains(body, "/register") {
		t.Error("login page links to a registration that is off")
	}
	a := e.adminBrowser(t)
	r := e.post(t, a, "/admin/settings/registration", url.Values{"enabled": {"on"}, "domains": {"example.org"}})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("settings: %d %s", r.StatusCode, readBody(r))
	}
	if _, body := e.get(t, c, "/login"); !strings.Contains(body, `href="/register"`) {
		t.Error("login page does not link to the registration")
	}
	if resp, body := e.get(t, c, "/register"); resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="email"`) || strings.Contains(body, `name="display_name"`) {
		t.Fatalf("form: %d", resp.StatusCode)
	}
	// wrong domain, short password, taken name
	for name, f := range map[string]url.Values{
		"domain":   {"username": {"neo"}, "email": {"neo@evil.test"}, "password": {"long enough password"}},
		"password": {"username": {"neo"}, "email": {"neo@example.org"}, "password": {"short"}},
		"taken":    {"username": {"user"}, "email": {"u@example.org"}, "password": {"long enough password"}},
		"email":    {"username": {"neo"}, "email": {"not an address"}, "password": {"long enough password"}},
	} {
		if r := e.post(t, c, "/register", f); r.StatusCode < 400 {
			t.Errorf("%s accepted: %d", name, r.StatusCode)
		}
	}
	r = e.post(t, c, "/register", url.Values{"username": {"neo"}, "email": {"neo@example.org"}, "password": {"long enough password"}})
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" || len(r.Cookies()) == 0 {
		t.Fatalf("sign-up: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	u, err := e.store.UserByUsername(ctx, e.store.Pool, "neo")
	if err != nil || u.Role != "user" || u.Email != "neo@example.org" || u.Disabled {
		t.Fatalf("created user %+v %v", u, err)
	}

	// with approval: no session, a note, and login stays closed until approved
	e.post(t, a, "/admin/settings/registration", url.Values{"enabled": {"on"}, "require_approval": {"on"}})
	c2 := e.browser()
	r = e.post(t, c2, "/register", url.Values{"username": {"trin"}, "email": {"trin@example.org"}, "password": {"long enough password"}})
	if body := readBody(r); r.StatusCode != http.StatusOK || !strings.Contains(body, "approve") || len(r.Cookies()) != 0 {
		t.Fatalf("approval flow: %d %v", r.StatusCode, r.Cookies())
	}
	if r := e.login(t, c2, "trin", "long enough password"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pending user logged in: %d", r.StatusCode)
	}
	_, users := e.get(t, a, "/admin/users")
	if !strings.Contains(users, "waiting for approval") {
		t.Fatal("pending user not flagged")
	}
	p, _ := e.store.UserByUsername(ctx, e.store.Pool, "trin")
	if r := e.post(t, a, "/admin/users/"+p.ID.String()+"/approve", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve: %d", r.StatusCode)
	}
	if r := e.login(t, c2, "trin", "long enough password"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("approved user cannot log in: %d", r.StatusCode)
	}
	if p, _ = e.store.UserByUsername(ctx, e.store.Pool, "trin"); p.PendingApproval {
		t.Error("still pending after approval")
	}

	// rate limit per address: the limit is 5 attempts in the test environment
	e.post(t, a, "/admin/settings/registration", url.Values{"enabled": {"on"}})
	var last int
	for range 8 {
		last = e.post(t, e.browser(), "/register", url.Values{"username": {"x"}, "email": {"bad"}, "password": {"p"}}).StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("no rate limit on sign-ups: last status %d", last)
	}
}

var png = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)

func multipartPost(t *testing.T, e *env, c *http.Client, path string, fields map[string]string, file []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	if file != nil {
		fw, _ := w.CreateFormFile("logo", "logo.png")
		fw.Write(file)
	}
	w.Close()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestNameAndLogo(t *testing.T) {
	e := newEnv(t)
	a := e.adminBrowser(t)
	if resp, _ := e.get(t, e.browser(), "/branding/logo"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("logo before upload: %d", resp.StatusCode)
	}
	// without a logo the favicon is the default mark
	if _, lb := e.get(t, e.browser(), "/login"); !strings.Contains(lb, `rel="icon" type="image/svg+xml" href="/static/favicon.svg"`) {
		t.Error("the page does not name the default favicon")
	}
	if fr, _ := e.get(t, e.browser(), "/favicon.ico"); fr.StatusCode != http.StatusTemporaryRedirect || fr.Header.Get("Location") != "/static/favicon.svg" {
		t.Errorf("/favicon.ico: %d %v", fr.StatusCode, fr.Header.Get("Location"))
	}
	if fr, fb := e.get(t, e.browser(), "/static/favicon.svg"); fr.StatusCode != http.StatusOK || !strings.Contains(fb, "<svg") {
		t.Errorf("the default favicon: %d", fr.StatusCode)
	}
	r := multipartPost(t, e, a, "/admin/settings/general", map[string]string{"name": "Makerspace DECT", "tagline": "phones for all"}, png)
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d %s", r.StatusCode, readBody(r))
	}
	e.get(t, a, "/") // brand cache is dropped on save, but be sure
	anon := e.browser()
	resp, body := e.get(t, anon, "/login")
	if !strings.Contains(body, "Makerspace DECT") || !strings.Contains(body, "/branding/logo?v=") || !strings.Contains(body, "phones for all") {
		t.Errorf("login page does not show the brand: %s", body[:min(len(body), 400)])
	}
	_ = resp
	// with one, the favicon is the logo
	if !strings.Contains(body, `<link rel="icon" href="/branding/logo?v=`) {
		t.Error("the page does not use the logo as its favicon")
	}
	if fr, _ := e.get(t, anon, "/favicon.ico"); fr.StatusCode != http.StatusTemporaryRedirect || fr.Header.Get("Location") != "/branding/logo" {
		t.Errorf("/favicon.ico with a logo: %d %v", fr.StatusCode, fr.Header.Get("Location"))
	}
	lr, lbody := e.get(t, anon, "/branding/logo")
	if lr.StatusCode != http.StatusOK || lr.Header.Get("Content-Type") != "image/png" || lbody != string(png) ||
		!strings.Contains(lr.Header.Get("Content-Security-Policy"), "sandbox") || lr.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("logo response: %d %v", lr.StatusCode, lr.Header)
	}
	// SVG and oversize are refused, and the old logo stays
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	if r := multipartPost(t, e, a, "/admin/settings/general", map[string]string{"name": "X"}, svg); r.StatusCode != http.StatusBadRequest {
		t.Errorf("svg accepted: %d", r.StatusCode)
	}
	if _, b := e.get(t, anon, "/branding/logo"); b != string(png) {
		t.Error("logo replaced by refused upload")
	}
	// a user cannot change it
	if r := multipartPost(t, e, e.userBrowser(t), "/admin/settings/general", map[string]string{"name": "Mine"}, nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("user changed the brand: %d", r.StatusCode)
	}
	// remove
	if r := multipartPost(t, e, a, "/admin/settings/general", map[string]string{"name": "Makerspace DECT", "remove_logo": "on"}, nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove: %d", r.StatusCode)
	}
	if resp, _ := e.get(t, anon, "/branding/logo"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("logo after removal: %d", resp.StatusCode)
	}
}

func TestMapTilesOpenTheCSPOnlyOnRequest(t *testing.T) {
	e := newEnv(t)
	a := e.adminBrowser(t)
	resp, _ := e.get(t, a, "/")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data:;") {
		t.Fatalf("default CSP: %s", csp)
	}
	if r := e.post(t, a, "/admin/settings/map", url.Values{"choice": {"custom"}, "tile_url": {"http://tiles.example.org/{z}/{x}/{y}.png"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("plain http tiles accepted: %d", r.StatusCode)
	}
	if r := e.post(t, a, "/admin/settings/map", url.Values{"choice": {"osm"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("osm: %d", r.StatusCode)
	}
	resp, _ = e.get(t, a, "/")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data: https://tile.openstreetmap.org;") {
		t.Errorf("CSP with OSM: %s", csp)
	}
	e.post(t, a, "/admin/settings/map", url.Values{"choice": {"none"}})
	resp, _ = e.get(t, a, "/")
	if csp := resp.Header.Get("Content-Security-Policy"); strings.Contains(csp, "openstreetmap") {
		t.Errorf("CSP still names OSM: %s", csp)
	}
}

func TestLoggingAdminAndReadOnlyUser(t *testing.T) {
	e := newEnv(t)
	a, u := e.adminBrowser(t), e.userBrowser(t)
	_, body := e.get(t, u, "/logging")
	if !strings.Contains(body, "Only an administrator can change this") || strings.Contains(body, `name="default"`) {
		t.Error("user sees controls")
	}
	if resp, _ := e.get(t, u, "/admin/logging"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("user on admin logging: %d", resp.StatusCode)
	}
	if r := e.post(t, u, "/admin/logging", url.Values{"level_sip": {"debug"}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("user set a level: %d", r.StatusCode)
	}
	r := e.post(t, a, "/admin/logging", url.Values{"default": {"warn"}, "level_sip": {"calls"}, "orig_sip": {"default"}, "level_web": {"default"}, "ttl": {"1h"}})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("set: %d %s", r.StatusCode, readBody(r))
	}
	if s := (&e2{}).state(e); s.Default != "warn" || s.Components["sip"].Level != "calls" || s.Components["sip"].Until == nil {
		t.Errorf("state %+v", s)
	}
	_, body = e.get(t, u, "/logging")
	if !strings.Contains(body, "calls") || !strings.Contains(body, "default (warn)") {
		t.Errorf("user does not see what the admin set: %s", body)
	}
	// persisted and audited
	_, aud := e.get(t, a, "/admin/audit")
	if !strings.Contains(aud, "log.level") {
		t.Error("level change not audited")
	}
	// bad input is refused and changes nothing
	if r := e.post(t, a, "/admin/logging", url.Values{"level_sip": {"shout"}, "ttl": {"1h"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("bad level: %d", r.StatusCode)
	}
	if r := e.post(t, a, "/admin/logging", url.Values{"level_sip": {"debug"}, "ttl": {"forever"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("bad ttl: %d", r.StatusCode)
	}
}

type e2 struct{}

func (*e2) state(e *env) (st struct {
	Default    string
	Components map[string]struct {
		Level string
		Until any
	}
}) {
	// read back through the persisted setting, as a restart would
	var raw []byte
	_ = e.store.Pool.QueryRow(ctx, "SELECT value FROM setting WHERE key = 'logging'").Scan(&raw)
	_ = jsonUnmarshal(raw, &st)
	return st
}

func TestLogNotificationOptIn(t *testing.T) {
	e := newEnv(t)
	u := e.userBrowser(t)
	// the test user has no e-mail address: the opt-in is refused
	if r := e.post(t, u, "/logging/notify", url.Values{"notify": {"on"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("opt-in without an address: %d", r.StatusCode)
	}
	if _, err := e.store.Pool.Exec(ctx, "UPDATE app_user SET email = 'u@example.org' WHERE username = 'user'"); err != nil {
		t.Fatal(err)
	}
	// the session user is read once per request, so log in again
	u = e.userBrowser(t)
	if r := e.post(t, u, "/logging/notify", url.Values{"notify": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("opt-in: %d", r.StatusCode)
	}
	var on bool
	e.store.Pool.QueryRow(ctx, "SELECT notify_log_changes FROM app_user WHERE username = 'user'").Scan(&on)
	if !on {
		t.Fatal("opt-in not stored")
	}
	if r := e.post(t, u, "/logging/notify", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("opt-out: %d", r.StatusCode)
	}
	e.store.Pool.QueryRow(ctx, "SELECT notify_log_changes FROM app_user WHERE username = 'user'").Scan(&on)
	if on {
		t.Fatal("opt-out not stored")
	}
}

func TestStatisticsAreScoped(t *testing.T) {
	e := newEnv(t)
	var uid, aid uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'admin'").Scan(&aid)
	for i, o := range []uuid.UUID{uid, aid, aid, aid} {
		if _, err := e.store.Pool.Exec(ctx, `INSERT INTO call_log (owner_user_id, direction, started_at, answered_at, ended_at)
			VALUES ($1, 'out', now() - interval '1 hour', now() - interval '59 minutes', now() - interval '50 minutes')`, o); err != nil {
			t.Fatal(i, err)
		}
	}
	u, a := e.userBrowser(t), e.adminBrowser(t)
	_, ub := e.get(t, u, "/statistics")
	if !strings.Contains(ub, "Your handsets, accounts and calls only") || strings.Contains(ub, "The controller") {
		t.Error("user statistics page is not scoped")
	}
	if !strings.Contains(ub, `<div class="subheader">Calls, last 7 days</div><div class="h1 mb-0">1</div>`) {
		t.Errorf("user sees other people's calls: %s", ub)
	}
	if resp, _ := e.get(t, u, "/admin/statistics"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("user on /admin/statistics: %d", resp.StatusCode)
	}
	_, ab := e.get(t, a, "/admin/statistics")
	if !strings.Contains(ab, `<div class="h1 mb-0">4</div>`) || !strings.Contains(ab, "The controller") {
		t.Errorf("admin statistics: %s", ab[:min(len(ab), 600)])
	}
	if resp, body := e.get(t, a, "/admin"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Latest changes") {
		t.Errorf("dashboard: %d", resp.StatusCode)
	}
	if resp, body := e.get(t, a, "/admin/system"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "reachable") {
		t.Errorf("system: %d", resp.StatusCode)
	}
}

func TestStationProfile(t *testing.T) {
	e := newEnvWith(t, func(s *Server) {
		mk := testMK
		kr, err := db.LoadKeyring(ctx, s.Pool, mk, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Cells, err = cell.Open(ctx, s.Pool, kr, "dect.example.org:7300"); err != nil {
			t.Fatal(err)
		}
	})
	var uid uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	var mine, other uuid.UUID
	if err := e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state, enrolled_by) VALUES ('Attic', 'active', $1) RETURNING id", uid).Scan(&mine); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('Cellar', 'active') RETURNING id").Scan(&other); err != nil {
		t.Fatal(err)
	}
	u := e.userBrowser(t)
	_, list := e.get(t, u, "/stations")
	if !strings.Contains(list, "Attic") || strings.Contains(list, "Cellar") {
		t.Errorf("user sees the wrong stations: %s", list)
	}
	if resp, _ := e.get(t, u, "/stations/"+other.String()); resp.StatusCode != http.StatusNotFound {
		t.Errorf("someone else's station: %d", resp.StatusCode)
	}
	// the Base stations page is the same for an administrator: the ones they enrolled. Every cell is on the Cells page.
	a := e.adminBrowser(t)
	if _, alist := e.get(t, a, "/stations"); strings.Contains(alist, "Attic") || strings.Contains(alist, "Cellar") {
		t.Errorf("an administrator's own list shows other people's stations: %s", alist)
	}
	if _, all := e.get(t, a, "/cells"); !strings.Contains(all, "Attic") || !strings.Contains(all, "Cellar") {
		t.Errorf("the Cells page does not list every cell: %s", all)
	}
	// A cell that enrolled and never connected says so, and what to look at (a box without the chip firmware or with
	// the DECT service switched off enrols fine and then never connects); one that has been seen is just offline.
	if !strings.Contains(list, "never connected") {
		t.Errorf("the list does not say the cell never connected: %s", list)
	}
	if _, page := e.get(t, u, "/stations/"+mine.String()); !strings.Contains(page, "has not connected since") || !strings.Contains(page, "chip firmware") || !strings.Contains(page, "logread | grep opendectcell") {
		t.Errorf("the cell's page has no hint for a cell that never connected")
	}
	if _, err := e.store.Pool.Exec(ctx, "UPDATE cell SET last_seen_at = now() WHERE id = $1", mine); err != nil {
		t.Fatal(err)
	}
	if _, page := e.get(t, u, "/stations/"+mine.String()); strings.Contains(page, "has not connected since") || strings.Contains(page, "never connected") {
		t.Errorf("a cell that was seen is still reported as never connected")
	}
	if r := e.post(t, u, "/stations/"+other.String(), url.Values{"name": {"Mine now"}, "precision": {"exact"}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("edit of someone else's station: %d", r.StatusCode)
	}
	if resp, body := e.get(t, u, "/stations/"+mine.String()); resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="share_position"`) || strings.Contains(body, `name="share_position" checked`) {
		t.Errorf("profile page: %d (sharing must be off by default)", resp.StatusCode)
	}
	form := url.Values{"name": {"Attic 7520"}, "box_model": {"FRITZ!Box 7520"}, "lat": {"52,37593"}, "lon": {"9.73201"}, "precision": {"approx"},
		"share_name": {"on"}, "share_position": {"on"}}
	if r := e.post(t, u, "/stations/"+mine.String(), form); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d %s", r.StatusCode, readBody(r))
	}
	c, err := e.cells.GetOwned(ctx, mustUser(t, e, "user"), mine)
	if err != nil || c.Name != "Attic 7520" || c.Latitude == nil || *c.Latitude != 52.37593 || !c.ShareName || !c.SharePosition || c.ShareSpecs || c.ShareStats {
		t.Fatalf("stored %+v %v", c, err)
	}
	// position sharing without a position, half a position, out of range, taken name
	for name, f := range map[string]url.Values{
		"share without position": {"name": {"A"}, "precision": {"exact"}, "share_position": {"on"}},
		"half position":          {"name": {"A"}, "precision": {"exact"}, "lat": {"50"}},
		"out of range":           {"name": {"A"}, "precision": {"exact"}, "lat": {"95"}, "lon": {"1"}},
		"not a number":           {"name": {"A"}, "precision": {"exact"}, "lat": {"north"}, "lon": {"1"}},
		"bad precision":          {"name": {"A"}, "precision": {"street"}},
	} {
		if r := e.post(t, u, "/stations/"+mine.String(), f); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s accepted: %d", name, r.StatusCode)
		}
	}
	// names need not be unique
	if r := e.post(t, u, "/stations/"+mine.String(), url.Values{"name": {"Cellar"}, "precision": {"exact"}}); r.StatusCode != http.StatusSeeOther {
		t.Errorf("a duplicate name was refused: %d", r.StatusCode)
	}
	// the audit entry has the switches but not the coordinates
	_, aud := e.get(t, e.adminBrowser(t), "/admin/audit")
	if !strings.Contains(aud, "cell.profile") || strings.Contains(aud, "52.37593") {
		t.Error("audit lacks the profile change or leaks the position")
	}
	_ = io.Discard
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func TestPublicMapFeed(t *testing.T) {
	e := newEnvWith(t, func(s *Server) {
		mk := testMK
		kr, err := db.LoadKeyring(ctx, s.Pool, mk, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Cells, err = cell.Open(ctx, s.Pool, kr, "dect.example.org:7300"); err != nil {
			t.Fatal(err)
		}
	})
	q := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.store.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	q(`INSERT INTO cell (name, state, latitude, longitude, position_precision, share_position, share_name, box_model, share_specs)
		VALUES ('Public Attic', 'active', 52.37593, 9.73201, 'approx', true, true, 'FRITZ!Box 7520', false)`)
	q(`INSERT INTO cell (name, state, latitude, longitude, position_precision, share_position, share_stats) VALUES ('Quiet Cellar', 'active', 48.13743, 11.57549, 'exact', true, true)`)
	q(`INSERT INTO cell (name, state, latitude, longitude, share_position) VALUES ('Private Garage', 'active', 50.0, 8.0, false)`)
	q(`INSERT INTO cell (name, state, latitude, longitude, share_position) VALUES ('Disabled Shed', 'disabled', 51.0, 9.0, true)`)
	q(`INSERT INTO cell (name, state, share_name, box_model, share_specs) VALUES ('Name only', 'active', true, 'FRITZ!Box 7412', true)`)

	anon := e.browser()
	if resp, _ := e.get(t, anon, "/api/v1/public/stations"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("feed open before an admin allowed it: %d", resp.StatusCode)
	}
	if resp, _ := e.get(t, anon, "/map"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("map open before an admin allowed it: %d", resp.StatusCode)
	}
	// logged-in users see it regardless
	if resp, body := e.get(t, e.userBrowser(t), "/api/v1/public/stations"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "stations") {
		t.Errorf("feed for a user: %d", resp.StatusCode)
	}
	if r := e.post(t, e.adminBrowser(t), "/admin/settings/map", url.Values{"choice": {"none"}, "public": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("make public: %d", r.StatusCode)
	}
	resp, body := e.get(t, anon, "/api/v1/public/stations")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("feed: %d %v", resp.StatusCode, resp.Header)
	}
	var feed struct {
		Stations []map[string]any `json:"stations"`
	}
	if err := json.Unmarshal([]byte(body), &feed); err != nil {
		t.Fatal(err)
	}
	if len(feed.Stations) != 2 {
		t.Fatalf("want the two stations that share a position, got %d: %s", len(feed.Stations), body)
	}
	for _, st := range feed.Stations {
		switch st["name"] {
		case "Public Attic":
			if st["lat"] != 52.38 || st["lon"] != 9.73 {
				t.Errorf("position not blurred to about 1 km: %v %v", st["lat"], st["lon"])
			}
			if _, has := st["model"]; has {
				t.Error("specs shared with the switch off")
			}
			if _, has := st["stats"]; has {
				t.Error("statistics shared with the switch off")
			}
		case nil:
			// the station that shares no name: exact position, counts only
			if st["lat"] != 48.13743 {
				t.Errorf("exact position changed: %v", st["lat"])
			}
			if _, has := st["stats"]; !has {
				t.Error("statistics switched on but missing")
			}
		default:
			t.Errorf("unexpected station %v", st["name"])
		}
	}
	for _, leak := range []string{"Private Garage", "Disabled Shed", "Name only", "Quiet Cellar", "enrolled", "state"} {
		if strings.Contains(body, leak) {
			t.Errorf("feed mentions %q: %s", leak, body)
		}
	}
	if resp, page := e.get(t, anon, "/map"); resp.StatusCode != http.StatusOK || !strings.Contains(page, `id="public-map"`) {
		t.Errorf("public map page: %d", resp.StatusCode)
	}
}
