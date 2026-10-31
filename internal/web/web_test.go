// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/auth/oidctest"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/config"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
	"github.com/openDECTcell/opendectcell-controller/internal/logctl"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

var ctx = context.Background()

// one master key for the whole test process: every test database is opened with it
var testMK = func() secret.MasterKey {
	ks, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(ks)
	return mk
}()

type env struct {
	srv   *httptest.Server
	app   *Server
	store *auth.Store
	idp   *oidctest.Provider
	cells *cell.Store
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

func newEnvWith(t *testing.T, setup func(*Server)) *env {
	t.Helper()
	pool := dbtest.Migrated(t)
	store := &auth.Store{Pool: pool, Argon: auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1},
		SessionIdle: time.Hour, SessionMaxAge: 24 * time.Hour, RateLimit: 5}
	if store.Keyring, _ = db.LoadKeyring(ctx, pool, testMK, nil); store.Keyring == nil {
		t.Fatal("no keyring")
	}
	e := &env{store: store, idp: oidctest.New()}
	t.Cleanup(e.idp.Close)
	s := &Server{Pool: pool, Auth: store, LocalLogin: true, OIDCName: "Sign in with Test IdP",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test",
		Settings: &settings.Store{Pool: pool}, Stats: &stats.Store{Pool: pool}, Handsets: &handset.Store{Pool: pool}, LogCtl: logctl.New(slog.LevelInfo)}
	e.app = s
	e.srv = httptest.NewUnstartedServer(nil)
	base, _ := url.Parse("http://" + e.srv.Listener.Addr().String())
	s.BaseURL = base
	s.OIDC = &auth.OIDC{Store: store, RedirectURL: base.String() + "/auth/oidc/callback", Config: config.OIDC{
		Issuer: e.idp.Issuer(), ClientID: e.idp.ClientID, ClientSecret: e.idp.ClientSecret,
		Scopes: []string{"openid"}, GroupsClaim: "groups", AdminGroups: []string{"ops"},
	}}
	if setup != nil {
		setup(s)
		e.cells = s.Cells
	}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Config.Handler = h
	e.srv.Start()
	t.Cleanup(e.srv.Close)
	for _, u := range []auth.NewLocalUser{
		{Username: "admin", Password: "admin password", Role: auth.RoleAdmin},
		{Username: "user", Password: "user password", Role: auth.RoleUser},
	} {
		if _, err := store.CreateLocalUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// browser is a client with cookies that doesn't follow redirects and sends
// the headers a same-origin browser form post sends.
func (e *env) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *env) post(t *testing.T, c *http.Client, path string, form url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readBody(r *http.Response) string {
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	return string(b)
}

func testCSR(t *testing.T) []byte {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, k)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func (e *env) get(t *testing.T, c *http.Client, path string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (e *env) login(t *testing.T, c *http.Client, user, pw string) *http.Response {
	t.Helper()
	return e.post(t, c, "/login", url.Values{"username": {user}, "password": {pw}, "next": {"/account"}})
}

func TestHealthAndHeaders(t *testing.T) {
	e := newEnv(t)
	c := e.browser()
	for _, p := range []string{"/healthz", "/readyz"} {
		if resp, body := e.get(t, c, p); resp.StatusCode != http.StatusOK || body != "ok\n" {
			t.Errorf("%s: %d %q", p, resp.StatusCode, body)
		}
	}
	resp, _ := e.get(t, c, "/login")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("security headers: %v", resp.Header)
	}
	if resp, _ := e.get(t, c, "/static/app.css"); resp.StatusCode != http.StatusOK {
		t.Errorf("static: %d", resp.StatusCode)
	}
}

func TestLocalLoginFlow(t *testing.T) {
	e := newEnv(t)
	c := e.browser()

	resp, _ := e.get(t, c, "/account")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?next=%2Faccount") {
		t.Fatalf("anonymous: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if r := e.login(t, c, "user", "wrong"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad password: %d", r.StatusCode)
	}
	r := e.login(t, c, "user", "user password")
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/account" {
		t.Fatalf("login: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	ck := r.Cookies()[0]
	if !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Name != "opendectcell_session" {
		t.Errorf("cookie: %+v", ck)
	}
	resp, body := e.get(t, c, "/account")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "local account") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("account: %d", resp.StatusCode)
	}

	// A user does not see admin pages, and cannot tell they exist.
	for _, p := range []string{"/admin/users", "/admin/audit", "/admin/calls"} {
		if resp, _ := e.get(t, c, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("user on %s: %d", p, resp.StatusCode)
		}
	}

	// Logout ends the session server-side.
	if r := e.post(t, c, "/logout", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	sess, err := e.store.LookupSession(ctx, ck.Value)
	if err == nil {
		t.Fatalf("session alive after logout: %+v", sess)
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	e := newEnv(t)
	c := e.browser()
	e.login(t, c, "user", "user password")
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", resp.StatusCode)
	}
	if resp, _ := e.get(t, c, "/account"); resp.StatusCode != http.StatusOK {
		t.Fatal("cross-site POST logged the user out")
	}
}

func TestSessionFixation(t *testing.T) {
	e := newEnv(t)
	c := e.browser()
	u, _ := url.Parse(e.srv.URL)
	planted, _ := e.store.CreateSession(ctx, mustUser(t, e, "user"), "local")
	c.Jar.SetCookies(u, []*http.Cookie{{Name: "opendectcell_session", Value: planted}})
	r := e.login(t, c, "admin", "admin password")
	if got := r.Cookies()[0].Value; got == planted {
		t.Fatal("login kept the planted session token")
	}
	if _, err := e.store.LookupSession(ctx, planted); err == nil {
		t.Fatal("planted session still valid")
	}
}

func mustUser(t *testing.T, e *env, name string) *auth.User {
	u, err := e.store.UserByUsername(ctx, e.store.Pool, name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPasswordChange(t *testing.T) {
	e := newEnv(t)
	c := e.browser()
	e.login(t, c, "user", "user password")
	other := e.browser()
	e.login(t, other, "user", "user password")

	if r := e.post(t, c, "/account/password", url.Values{"current": {"nope"}, "new": {"new password!"}, "confirm": {"new password!"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong current: %d", r.StatusCode)
	}
	if r := e.post(t, c, "/account/password", url.Values{"current": {"user password"}, "new": {"new password!"}, "confirm": {"new password!"}}); r.StatusCode != http.StatusOK {
		t.Fatalf("change: %d", r.StatusCode)
	}
	if resp, _ := e.get(t, c, "/account"); resp.StatusCode != http.StatusOK {
		t.Error("current session ended")
	}
	if resp, _ := e.get(t, other, "/account"); resp.StatusCode != http.StatusSeeOther {
		t.Error("other session survived")
	}
	if r := e.login(t, e.browser(), "user", "new password!"); r.StatusCode != http.StatusSeeOther {
		t.Errorf("new password: %d", r.StatusCode)
	}
}

func TestOIDCBrowserFlowAndAudit(t *testing.T) {
	e := newEnv(t)
	e.idp.SetUser(oidctest.User{Subject: "sub-9", PreferredUsername: "frank", Name: "Frank", Groups: []string{"ops"}})
	c := e.browser()
	next := "/admin/audit"
	// Follow: controller → IdP → controller callback.
	loc := e.srv.URL + "/auth/oidc/start?next=" + url.QueryEscape(next)
	for i := 0; i < 3; i++ {
		resp, err := c.Get(loc)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("hop %d: %d", i, resp.StatusCode)
		}
		l, _ := resp.Location()
		loc = l.String()
	}
	if !strings.HasSuffix(loc, next) {
		t.Fatalf("landed at %s", loc)
	}
	resp, body := e.get(t, c, next)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin page as OIDC admin: %d", resp.StatusCode)
	}
	for _, want := range []string{"user.create", "user.login", "frank"} {
		if !strings.Contains(body, want) {
			t.Errorf("audit page lacks %q", want)
		}
	}
	rows, _ := audit.List(ctx, e.store.Pool, audit.Filter{})
	var addr string
	for _, r := range rows {
		if r.Action == "user.login" && r.ActorName == "frank" && r.RemoteAddr != nil {
			addr = r.RemoteAddr.String()
		}
	}
	if addr != "127.0.0.1" {
		t.Errorf("login audit address %q", addr)
	}
}

func TestClientAddr(t *testing.T) {
	s := &Server{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	for _, c := range []struct{ remote, xff, want string }{
		{"192.0.2.1:5000", "", "192.0.2.1"},
		{"192.0.2.1:5000", "203.0.113.9", "192.0.2.1"},                          // untrusted peer: header ignored
		{"10.1.1.1:5000", "203.0.113.9", "203.0.113.9"},                         // trusted proxy
		{"10.1.1.1:5000", "198.51.100.1, 203.0.113.9, 10.2.2.2", "203.0.113.9"}, // right-most untrusted
		{"10.1.1.1:5000", "garbage", "10.1.1.1"},
		{"[::ffff:192.0.2.5]:1", "", "192.0.2.5"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := s.clientAddr(r).String(); got != c.want {
			t.Errorf("%s / %q: %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func newUser(name string) auth.NewLocalUser {
	return auth.NewLocalUser{Username: name, Password: name + " password!", Role: auth.RoleUser}
}
