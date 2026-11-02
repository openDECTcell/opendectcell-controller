// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/auth/oidctest"
	"github.com/openDECTcell/opendectcell-controller/internal/config"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
)

var ctx = context.Background()

// Cheap parameters: tests are about behaviour, not cost.
var fast = auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1}

func TestPasswordHash(t *testing.T) {
	h, err := auth.HashPassword(fast, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("format: %s", h)
	}
	if ok, err := auth.VerifyPassword(h, "correct horse"); !ok || err != nil {
		t.Errorf("right password: %v %v", ok, err)
	}
	if ok, _ := auth.VerifyPassword(h, "correct horsf"); ok {
		t.Error("wrong password accepted")
	}
	if _, err := auth.VerifyPassword("$argon2id$v=19$m=99999999,t=1,p=1$AA$AA", "x"); err == nil {
		t.Error("absurd parameters accepted")
	}
	if !auth.NeedsRehash(h, auth.ArgonParams{MemoryKiB: 128, Time: 1, Threads: 1}) || auth.NeedsRehash(h, fast) {
		t.Error("NeedsRehash")
	}
}

func TestSafeReturnPath(t *testing.T) {
	for in, want := range map[string]string{
		"/handsets?x=1": "/handsets?x=1", "": "/", "https://evil.example/": "/",
		"//evil.example": "/", "/\\evil.example": "/", "handsets": "/", "/a\r\nSet-Cookie: x": "/",
	} {
		if got := auth.SafeReturnPath(in); got != want {
			t.Errorf("SafeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapRole(t *testing.T) {
	cfg := config.OIDC{AdminGroups: []string{"ops"}, UserGroups: []string{"staff"}}
	for _, c := range []struct {
		groups []string
		role   auth.Role
		ok     bool
	}{
		{[]string{"staff", "ops"}, auth.RoleAdmin, true},
		{[]string{"staff"}, auth.RoleUser, true},
		{[]string{"guests"}, "", false},
		{nil, "", false},
	} {
		r, ok := auth.MapRole(cfg, c.groups)
		if r != c.role || ok != c.ok {
			t.Errorf("%v: %q %v", c.groups, r, ok)
		}
	}
	if r, ok := auth.MapRole(config.OIDC{}, nil); r != auth.RoleUser || !ok {
		t.Error("empty user groups should admit everyone as user")
	}
}

func TestAuthorisationRules(t *testing.T) {
	a := &auth.User{ID: uuid.New(), Role: auth.RoleAdmin}
	u := &auth.User{ID: uuid.New(), Role: auth.RoleUser}
	other := uuid.New()
	switch {
	case !auth.CanAccess(a, &other), !auth.CanAccess(a, nil):
		t.Error("admin denied")
	case !auth.CanAccess(u, &u.ID):
		t.Error("owner denied")
	case auth.CanAccess(u, &other), auth.CanAccess(u, nil):
		t.Error("user reached someone else's or a shared object")
	case auth.CanAccess(nil, &other):
		t.Error("anonymous allowed")
	case auth.CanAddCells(u), !auth.CanAddCells(&auth.User{MayAddCells: true}), !auth.CanAddCells(a):
		t.Error("CanAddCells")
	}
	u.Disabled = true
	if auth.CanAccess(u, &u.ID) {
		t.Error("disabled user allowed")
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(t *testing.T) (*auth.Store, *clock) {
	c := &clock{time.Now()}
	return &auth.Store{
		Pool: dbtest.Migrated(t), Argon: fast,
		SessionIdle: time.Hour, SessionMaxAge: 24 * time.Hour,
		RateLimit: 3, Now: c.now,
	}, c
}

func TestLocalLoginAndSessions(t *testing.T) {
	s, clk := newStore(t)
	u, err := s.CreateLocalUser(ctx, auth.NewLocalUser{Username: "Alice", Password: "long enough pw", Role: auth.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLocalUser(ctx, auth.NewLocalUser{Username: "alice", Password: "long enough pw", Role: auth.RoleUser}); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Fatalf("case-insensitive duplicate: %v", err)
	}
	if _, err := s.CreateLocalUser(ctx, auth.NewLocalUser{Username: "bob", Password: "short", Role: auth.RoleUser}); err == nil {
		t.Fatal("short password accepted")
	}

	got, err := s.LoginLocal(ctx, "ALICE", "long enough pw", "192.0.2.1")
	if err != nil || got.ID != u.ID || got.LastLoginAt == nil {
		t.Fatalf("login: %+v %v", got, err)
	}
	tok, err := s.CreateSession(ctx, got, "local")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.LookupSession(ctx, tok)
	if err != nil || sess.User.ID != u.ID || !sess.User.IsAdmin() {
		t.Fatalf("lookup: %+v %v", sess, err)
	}
	var stored int
	s.Pool.QueryRow(ctx, "SELECT count(*) FROM session WHERE token_hash = $1", []byte(tok)).Scan(&stored)
	if stored != 0 {
		t.Fatal("token stored in clear")
	}

	// Idle expiry, then absolute expiry despite activity.
	clk.t = clk.t.Add(59 * time.Minute)
	if _, err := s.LookupSession(ctx, tok); err != nil {
		t.Fatalf("within idle: %v", err)
	}
	clk.t = clk.t.Add(61 * time.Minute)
	if _, err := s.LookupSession(ctx, tok); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("idle session survived: %v", err)
	}
	tok, _ = s.CreateSession(ctx, got, "local")
	for i := range 25 {
		clk.t = clk.t.Add(59 * time.Minute)
		if _, err := s.LookupSession(ctx, tok); err != nil {
			if i < 23 {
				t.Fatalf("expired early at step %d: %v", i, err)
			}
			break
		}
		if i == 24 {
			t.Fatal("session outlived its maximum age")
		}
	}

	// Password change ends other sessions but keeps the current one.
	keep, _ := s.CreateSession(ctx, got, "local")
	other, _ := s.CreateSession(ctx, got, "local")
	if err := s.SetPassword(ctx, u.ID, "another long pw", auth.HashToken(keep)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupSession(ctx, keep); err != nil {
		t.Errorf("current session ended: %v", err)
	}
	if _, err := s.LookupSession(ctx, other); err == nil {
		t.Error("other session survived the password change")
	}
	if _, err := s.LoginLocal(ctx, "alice", "long enough pw", ""); !errors.Is(err, auth.ErrBadLogin) {
		t.Errorf("old password: %v", err)
	}
}

func TestLoginRateLimitAndDisabled(t *testing.T) {
	s, clk := newStore(t)
	s.CreateLocalUser(ctx, auth.NewLocalUser{Username: "carol", Password: "long enough pw", Role: auth.RoleUser})
	for i := range 3 {
		if _, err := s.LoginLocal(ctx, "carol", "wrong password", "192.0.2.9"); !errors.Is(err, auth.ErrBadLogin) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := s.LoginLocal(ctx, "carol", "long enough pw", "198.51.100.1"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("username not limited: %v", err)
	}
	if _, err := s.LoginLocal(ctx, "nobody", "x", "192.0.2.9"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("address not limited: %v", err)
	}
	clk.t = clk.t.Add(16 * time.Minute)
	if _, err := s.LoginLocal(ctx, "carol", "long enough pw", "192.0.2.9"); err != nil {
		t.Fatalf("after window: %v", err)
	}
	// Unknown user: same error as a wrong password.
	if _, err := s.LoginLocal(ctx, "nobody", "long enough pw", ""); !errors.Is(err, auth.ErrBadLogin) {
		t.Errorf("unknown user: %v", err)
	}
	s.Pool.Exec(ctx, "UPDATE app_user SET disabled = true WHERE username = 'carol'")
	if _, err := s.LoginLocal(ctx, "carol", "long enough pw", ""); !errors.Is(err, auth.ErrBadLogin) {
		t.Errorf("disabled user logged in: %v", err)
	}

	rows, _ := audit.List(ctx, s.Pool, audit.Filter{Limit: 100})
	actions := map[string]int{}
	for _, r := range rows {
		actions[r.Action]++
		for k, v := range r.Details {
			if strings.Contains(strings.ToLower(k), "password") || v == "wrong password" {
				t.Errorf("password in audit details: %v", r.Details)
			}
		}
	}
	if actions["user.login_failed"] < 3 || actions["user.login_ratelimited"] < 2 || actions["user.login"] != 1 {
		t.Errorf("audit actions: %v", actions)
	}
}

// runOIDC drives one browser login through the provider and returns the
// callback's result.
func runOIDC(t *testing.T, o *auth.OIDC, returnTo string) (*auth.User, string, error) {
	t.Helper()
	start, bind, err := o.Begin(ctx, returnTo)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(start)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	return o.Callback(ctx, loc.Query().Get("state"), loc.Query().Get("code"), bind)
}

func TestOIDCLogin(t *testing.T) {
	p := oidctest.New()
	defer p.Close()
	s, _ := newStore(t)
	o := &auth.OIDC{Store: s, RedirectURL: "http://controller.test/auth/oidc/callback", Config: config.OIDC{
		Issuer: p.Issuer(), ClientID: p.ClientID, ClientSecret: p.ClientSecret,
		Scopes: []string{"openid", "profile", "email"}, GroupsClaim: "groups",
		AdminGroups: []string{"dect-admins"}, UserGroups: []string{"staff"},
	}}

	p.SetUser(oidctest.User{Subject: "s-1", PreferredUsername: "dave", Name: "Dave", Email: "dave@example.org", Groups: []string{"staff"}})
	u, back, err := runOIDC(t, o, "/handsets")
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "dave" || u.Role != auth.RoleUser || u.AuthSource != "oidc" || back != "/handsets" {
		t.Fatalf("first login: %+v %q", u, back)
	}

	// Next login: groups changed → role follows; same user row.
	p.SetUser(oidctest.User{Subject: "s-1", PreferredUsername: "dave", Groups: []string{"staff", "dect-admins"}})
	u2, _, err := runOIDC(t, o, "//evil.example")
	if err != nil || u2.ID != u.ID || u2.Role != auth.RoleAdmin {
		t.Fatalf("second login: %+v %v", u2, err)
	}

	// No matching group → refused, no user created.
	p.SetUser(oidctest.User{Subject: "s-2", PreferredUsername: "eve", Groups: []string{"guests"}})
	if _, _, err := runOIDC(t, o, "/"); !errors.Is(err, auth.ErrNotAuthorized) {
		t.Fatalf("unmapped user: %v", err)
	}
	if _, err := s.UserByUsername(ctx, s.Pool, "eve"); !errors.Is(err, auth.ErrNotFound) {
		t.Error("unmapped user was created")
	}

	// An IdP user claiming a local account's name gets a suffixed name.
	s.CreateLocalUser(ctx, auth.NewLocalUser{Username: "root", Password: "long enough pw", Role: auth.RoleAdmin})
	p.SetUser(oidctest.User{Subject: "s-3", PreferredUsername: "root", Groups: []string{"staff"}})
	u3, _, err := runOIDC(t, o, "/")
	if err != nil || u3.Username == "root" || !strings.HasPrefix(u3.Username, "root-") || u3.Role != auth.RoleUser {
		t.Fatalf("name collision: %+v %v", u3, err)
	}

	// Replayed state and a bad nonce are refused.
	p.SetUser(oidctest.User{Subject: "s-1", Groups: []string{"staff"}})
	start, bind, _ := o.Begin(ctx, "/")
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := c.Get(start)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	// the callback of somebody else's browser, or with no binding at all, is refused and does not use the login up
	if _, _, err := o.Callback(ctx, loc.Query().Get("state"), loc.Query().Get("code"), "another browser's token"); !errors.Is(err, auth.ErrOIDCState) {
		t.Errorf("a callback from another browser: %v", err)
	}
	if _, _, err := o.Callback(ctx, loc.Query().Get("state"), loc.Query().Get("code"), ""); !errors.Is(err, auth.ErrOIDCState) {
		t.Errorf("a callback without the binding: %v", err)
	}
	if _, _, err := o.Callback(ctx, loc.Query().Get("state"), loc.Query().Get("code"), bind); err != nil {
		t.Fatal(err)
	}
	if _, _, err := o.Callback(ctx, loc.Query().Get("state"), loc.Query().Get("code"), bind); !errors.Is(err, auth.ErrOIDCState) {
		t.Errorf("replayed state: %v", err)
	}
	p.WrongNonce = true
	if _, _, err := runOIDC(t, o, "/"); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Errorf("wrong nonce: %v", err)
	}
}

func TestUserAdministration(t *testing.T) {
	s, _ := newStore(t)
	adminCtx := ctx
	admin, _ := s.CreateLocalUser(adminCtx, auth.NewLocalUser{Username: "boss", Password: "long enough pw", Role: auth.RoleAdmin})
	u, _ := s.CreateLocalUser(adminCtx, auth.NewLocalUser{Username: "dora", Password: "long enough pw", Role: auth.RoleUser})
	tok, _ := s.CreateSession(ctx, u, "local")

	if _, err := s.UpdateUser(ctx, u, admin.ID, auth.UserUpdate{Disabled: ptr(true)}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("user acting as admin: %v", err)
	}
	for _, up := range []auth.UserUpdate{{Role: ptr(auth.RoleUser)}, {Disabled: ptr(true)}} {
		if _, err := s.UpdateUser(ctx, admin, admin.ID, up); !errors.Is(err, auth.ErrSelf) {
			t.Fatalf("admin demoting/disabling self: %v", err)
		}
	}
	got, err := s.UpdateUser(ctx, admin, u.ID, auth.UserUpdate{Role: ptr(auth.RoleAdmin), MayAddCells: ptr(true), Email: ptr("dora@example.org")})
	if err != nil || got.Role != auth.RoleAdmin || !got.MayAddCells || got.Email != "dora@example.org" {
		t.Fatalf("update: %+v %v", got, err)
	}
	if _, err := s.UpdateUser(ctx, admin, u.ID, auth.UserUpdate{Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupSession(ctx, tok); err == nil {
		t.Fatal("disabled user's session survived")
	}
	// Deleting: not oneself, not while owning things.
	if err := s.DeleteUser(ctx, admin, admin.ID); !errors.Is(err, auth.ErrSelf) {
		t.Fatalf("self delete: %v", err)
	}
	s.Pool.Exec(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'x', 'u', 'd')", u.ID)
	if err := s.DeleteUser(ctx, admin, u.ID); !errors.Is(err, auth.ErrHasObjects) {
		t.Fatalf("owner delete: %v", err)
	}
	s.Pool.Exec(ctx, "DELETE FROM sip_account")
	if err := s.DeleteUser(ctx, admin, u.ID); err != nil {
		t.Fatal(err)
	}
	rows, _ := audit.List(ctx, s.Pool, audit.Filter{ObjectType: "user", ObjectID: u.ID.String()})
	var actions []string
	for _, r := range rows {
		actions = append(actions, r.Action)
	}
	if !strings.Contains(strings.Join(actions, ","), "user.delete") || !strings.Contains(strings.Join(actions, ","), "user.update") {
		t.Errorf("audit: %v", actions)
	}
}

func ptr[T any](v T) *T { return &v }
