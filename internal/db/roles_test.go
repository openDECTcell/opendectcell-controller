// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

func TestRuntimeCredentials(t *testing.T) {
	u, pw, err := db.RuntimeCredentials("postgres://opendectcell_app:s3cr%40t@db:5432/opendectcell?sslmode=disable")
	if err != nil || u != "opendectcell_app" || pw != "s3cr@t" {
		t.Fatalf("%q %q %v", u, pw, err)
	}
	if _, _, err := db.RuntimeCredentials("postgres://db/opendectcell"); err == nil {
		t.Fatal("a URL without a user was accepted")
	}
}

// urlFor is the URL of the pool's database for another role.
func urlFor(p *pgxpool.Pool, role, pw string) string {
	c := p.Config().ConnConfig
	u := url.URL{Scheme: "postgres", User: url.UserPassword(role, pw), Host: c.Host + ":" + strconv.Itoa(int(c.Port)), Path: "/" + c.Database, RawQuery: "sslmode=disable"}
	return u.String()
}

// The runtime role does everything the controller does, and nothing that changes the schema or reaches beyond the
// database.
func TestRuntimeRoleHasTableRightsOnly(t *testing.T) {
	ctx := context.Background()
	admin := dbtest.MigratedAsOwner(t)
	var b [5]byte
	rand.Read(b[:])
	role := "dwtest_app_" + hex.EncodeToString(b[:])
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP OWNED BY "+pgx.Identifier{role}.Sanitize())
		_, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
	})
	if super, err := db.IsSuperuser(ctx, admin); err != nil || !super {
		t.Fatalf("the test administrator should be a superuser: %v %v", super, err)
	}
	if err := db.EnsureRuntimeRole(ctx, admin, role, "first password"); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantRuntime(ctx, admin, role); err != nil {
		t.Fatal(err)
	}
	// the password is re-set when it changes (the URL is the truth), and running it again is harmless
	if err := db.EnsureRuntimeRole(ctx, admin, role, "pass'word \"two\""); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := db.GrantRuntime(ctx, admin, role); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Open(ctx, urlFor(admin, role, "first password"), 1); err == nil {
		t.Fatal("the old password still works")
	}
	rt, err := db.Open(ctx, urlFor(admin, role, "pass'word \"two\""), 4)
	if err != nil {
		t.Fatalf("the runtime role cannot connect: %v", err)
	}
	defer rt.Close()
	if super, _ := db.IsSuperuser(ctx, rt); super {
		t.Fatal("the runtime role is a superuser")
	}

	// What the controller does: keys, users, sessions.
	ks, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(ks)
	if _, err := db.LoadKeyring(ctx, rt, mk, nil); err != nil {
		t.Fatalf("keyring as the runtime role: %v", err)
	}
	st := &auth.Store{Pool: rt, Argon: auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1}, SessionIdle: 3600e9, SessionMaxAge: 3600e9, RateLimit: 5}
	u, err := st.CreateLocalUser(ctx, auth.NewLocalUser{Username: "rt", Password: "long enough pw", Role: auth.RoleAdmin})
	if err != nil {
		t.Fatalf("create a user as the runtime role: %v", err)
	}
	if _, err := st.LoginLocal(ctx, "rt", "long enough pw", "192.0.2.1"); err != nil {
		t.Fatalf("log in as the runtime role: %v", err)
	}
	if _, err := st.CreateSession(ctx, u, "local"); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.NewMigrator(rt); m != nil {
		if cur, latest, err := m.Status(ctx); err != nil || cur != latest {
			t.Fatalf("the runtime role cannot read the migration status: %d/%d %v", cur, latest, err)
		}
	}

	// What it must not do.
	for _, q := range []string{
		"CREATE TABLE evil (x int)",
		"DROP TABLE app_user",
		"ALTER TABLE app_user ADD COLUMN evil int",
		"TRUNCATE schema_migration",
		"CREATE ROLE evil LOGIN",
		"CREATE DATABASE evil",
		"SELECT pg_read_file('/etc/passwd')",
		"COPY app_user TO PROGRAM 'true'",
		"CREATE FUNCTION evil() RETURNS int LANGUAGE sql AS 'SELECT 1'",
	} {
		if _, err := rt.Exec(ctx, q); err == nil {
			t.Errorf("the runtime role could run: %s", q)
		} else if !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "must be") {
			t.Errorf("%s: unexpected error %v", q, err)
		}
	}
}
