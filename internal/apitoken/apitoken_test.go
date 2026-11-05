// SPDX-License-Identifier: AGPL-3.0-or-later

package apitoken_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/apitoken"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
)

var ctx = context.Background()

type fixture struct {
	s         *apitoken.Store
	anna, ben uuid.UUID
	now       time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := dbtest.Migrated(t)
	f := &fixture{now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	f.s = &apitoken.Store{Pool: pool, Now: func() time.Time { return f.now }}
	for name, id := range map[string]*uuid.UUID{"anna": &f.anna, "ben": &f.ben} {
		if err := pool.QueryRow(ctx, "INSERT INTO app_user (username, role, auth_source) VALUES ($1, 'user', 'local') RETURNING id", name).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestCreateAndAuthenticate(t *testing.T) {
	f := newFixture(t)
	tok, rec, err := f.s.Create(ctx, f.anna, "  Home Assistant ", []string{"calls:read", "statistics:read", "calls:read"}, 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "odc_") || len(tok) != 4+16+1+43 || rec.Name != "Home Assistant" || !strings.HasPrefix(tok, "odc_"+rec.Lookup+"_") || rec.Display() != "odc_"+rec.Lookup+"_…" {
		t.Fatalf("token %q record %+v", tok, rec)
	}
	if strings.Join(rec.Scopes, ",") != "calls:read,statistics:read" {
		t.Errorf("scopes sorted and without duplicates: %v", rec.Scopes)
	}
	if !rec.ExpiresAt.Equal(f.now.Add(90 * 24 * time.Hour)) {
		t.Errorf("expires %v", rec.ExpiresAt)
	}
	// only a hash is kept: the secret is nowhere in the database
	secret := tok[strings.LastIndex(tok, "_")+1:]
	var n int
	f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM api_token WHERE secret_hash = $1 OR name LIKE '%'||$2||'%'", []byte(secret), secret).Scan(&n)
	f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE details::text LIKE '%'||$1||'%'", secret).Scan(&n)
	if n != 0 {
		t.Error("the secret is in the audit log")
	}

	got, err := f.s.Authenticate(ctx, tok, "192.0.2.7")
	if err != nil || got.UserID != f.anna || got.ID != rec.ID || !got.Has("calls:read") || got.Has("calls:write") || got.Name != "Home Assistant" {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	// the use is noted, at most once a minute
	var ip string
	var used *time.Time
	f.s.Pool.QueryRow(ctx, "SELECT last_used_at, last_used_ip FROM api_token WHERE id = $1", rec.ID).Scan(&used, &ip)
	if used == nil || !used.Equal(f.now) || ip != "192.0.2.7" {
		t.Errorf("last use: %v %q", used, ip)
	}
	f.now = f.now.Add(20 * time.Second)
	f.s.Authenticate(ctx, tok, "192.0.2.8")
	f.s.Pool.QueryRow(ctx, "SELECT last_used_ip FROM api_token WHERE id = $1", rec.ID).Scan(&ip)
	if ip != "192.0.2.7" {
		t.Errorf("written again within a minute: %q", ip)
	}
	f.now = f.now.Add(time.Minute)
	f.s.Authenticate(ctx, tok, "192.0.2.8")
	f.s.Pool.QueryRow(ctx, "SELECT last_used_ip FROM api_token WHERE id = $1", rec.ID).Scan(&ip)
	if ip != "192.0.2.8" {
		t.Errorf("not written after a minute: %q", ip)
	}

	// every failure is the same error
	lookup := rec.Lookup
	bad := []string{"", "x", tok + "x", tok[:len(tok)-1], "odc_" + lookup + "_" + strings.Repeat("A", 43), "odc_" + strings.Repeat("0", 16) + "_" + secret,
		"DWT_" + lookup + "_" + secret, "odc_" + strings.ToUpper(lookup) + "_" + secret, "odc_" + lookup + "-" + secret, "Bearer " + tok, " " + tok}
	for _, b := range bad {
		if _, err := f.s.Authenticate(ctx, b, ""); !errors.Is(err, apitoken.ErrInvalid) {
			t.Errorf("%q: %v", b, err)
		}
	}
	// expired
	f.now = rec.ExpiresAt
	if _, err := f.s.Authenticate(ctx, tok, ""); !errors.Is(err, apitoken.ErrInvalid) {
		t.Errorf("expired: %v", err)
	}
	f.now = rec.ExpiresAt.Add(-time.Second)
	if _, err := f.s.Authenticate(ctx, tok, ""); err != nil {
		t.Errorf("a second before it expires: %v", err)
	}
}

func TestValidation(t *testing.T) {
	f := newFixture(t)
	day := 24 * time.Hour
	for name, c := range map[string]struct {
		n      string
		scopes []string
		life   time.Duration
	}{
		"no name":     {"", []string{"calls:read"}, 30 * day},
		"blank name":  {"   ", []string{"calls:read"}, 30 * day},
		"long name":   {strings.Repeat("x", 65), []string{"calls:read"}, 30 * day},
		"control":     {"a\nb", []string{"calls:read"}, 30 * day},
		"no scope":    {"x", nil, 30 * day},
		"unknown":     {"x", []string{"admin:everything"}, 30 * day},
		"one unknown": {"x", []string{"calls:read", "calls:rread"}, 30 * day},
		"too short":   {"x", []string{"calls:read"}, time.Hour},
		"too long":    {"x", []string{"calls:read"}, 400 * day},
		"never":       {"x", []string{"calls:read"}, 0},
	} {
		_, _, err := f.s.Create(ctx, f.anna, c.n, c.scopes, c.life)
		var v apitoken.ValidationError
		if !errors.As(err, &v) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := f.s.Create(ctx, f.anna, "x", []string{"calls:read"}, 366*day); err != nil {
		t.Errorf("a year and a day: %v", err)
	}
	for _, sc := range apitoken.Scopes {
		if !apitoken.ValidScope(sc.Name) || strings.HasPrefix(sc.Name, "admin") {
			t.Errorf("scope %q", sc.Name)
		}
	}
	// at most 20 at a time (one is made above); a revoked one does not count
	var last apitoken.Record
	for i := 0; i < apitoken.MaxPerUser-1; i++ {
		var err error
		if _, last, err = f.s.Create(ctx, f.anna, "t", []string{"calls:read"}, 30*day); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, _, err := f.s.Create(ctx, f.anna, "one more", []string{"calls:read"}, 30*day); err == nil {
		t.Error("a 21st token was made")
	}
	if _, _, err := f.s.Create(ctx, f.ben, "ben's", []string{"calls:read"}, 30*day); err != nil {
		t.Errorf("ben is limited by anna's tokens: %v", err)
	}
	if err := f.s.Revoke(ctx, f.anna, last.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.Create(ctx, f.anna, "one more", []string{"calls:read"}, 30*day); err != nil {
		t.Errorf("after a revoke: %v", err)
	}
}

func TestRevokeAndListAreTheOwners(t *testing.T) {
	f := newFixture(t)
	tok, rec, _ := f.s.Create(ctx, f.anna, "mine", []string{"calls:read"}, 30*24*time.Hour)
	if err := f.s.Revoke(ctx, f.ben, rec.ID); !errors.Is(err, apitoken.ErrNotFound) {
		t.Errorf("ben revoked anna's token: %v", err)
	}
	if _, err := f.s.Authenticate(ctx, tok, ""); err != nil {
		t.Fatalf("the token stopped working: %v", err)
	}
	if l, _ := f.s.List(ctx, f.ben); len(l) != 0 {
		t.Errorf("ben sees %d tokens", len(l))
	}
	if err := f.s.Revoke(ctx, f.anna, rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Revoke(ctx, f.anna, rec.ID); err != nil {
		t.Errorf("a second revoke: %v", err)
	}
	if err := f.s.Revoke(ctx, f.anna, uuid.New()); !errors.Is(err, apitoken.ErrNotFound) {
		t.Errorf("an unknown token: %v", err)
	}
	if _, err := f.s.Authenticate(ctx, tok, ""); !errors.Is(err, apitoken.ErrInvalid) {
		t.Errorf("a revoked token works: %v", err)
	}
	l, _ := f.s.List(ctx, f.anna)
	if len(l) != 1 || l[0].RevokedAt == nil || l[0].Active(f.now) {
		t.Errorf("list: %+v", l)
	}
	var revokes, creates int
	f.s.Pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE action = 'api_token.revoke'), count(*) FILTER (WHERE action = 'api_token.create') FROM audit_log").Scan(&revokes, &creates)
	if revokes != 1 || creates != 1 {
		t.Errorf("audit: %d revokes (the second is not one), %d creates", revokes, creates)
	}
	// deleting the user deletes the tokens
	f.s.Pool.Exec(ctx, "DELETE FROM app_user WHERE id = $1", f.anna)
	var n int
	f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM api_token").Scan(&n)
	if n != 0 {
		t.Errorf("%d tokens of a deleted user", n)
	}
}

// A token that has expired is not one of the 20.
func TestExpiredTokensDoNotCountAgainstTheLimit(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < apitoken.MaxPerUser; i++ {
		if _, _, err := f.s.Create(ctx, f.anna, "short", []string{"calls:read"}, 24*time.Hour); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, _, err := f.s.Create(ctx, f.anna, "x", []string{"calls:read"}, 24*time.Hour); err == nil {
		t.Fatal("a 21st token")
	}
	f.now = f.now.Add(48 * time.Hour)
	if _, _, err := f.s.Create(ctx, f.anna, "x", []string{"calls:read"}, 24*time.Hour); err != nil {
		t.Errorf("after the others expired: %v", err)
	}
}
