// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dbtest gives tests a fresh, empty PostgreSQL database.
//
// Set OPENDECTCELL_TEST_DATABASE_URL to a server where the user may create
// databases. Without it, database tests are skipped locally, and fail in CI
// (CI=true), so they can never be skipped silently there.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/db"
)

// EnvVar names the admin connection URL.
const EnvVar = "OPENDECTCELL_TEST_DATABASE_URL"

// New creates an empty database, returns a pool on it, and drops it when the
// test ends.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv(EnvVar)
	if admin == "" {
		if os.Getenv("CI") == "true" {
			t.Fatalf("%s is not set in CI", EnvVar)
		}
		t.Skipf("%s not set; skipping database test", EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("dbtest: connect: %v", err)
	}
	defer conn.Close(ctx)
	var b [6]byte
	rand.Read(b[:])
	name := "opendectcell_test_" + hex.EncodeToString(b[:])
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("dbtest: create database: %v", err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("dbtest: %v", err)
	}
	u.Path = "/" + name
	pool, err := db.Open(ctx, u.String(), 5)
	if err != nil {
		t.Fatalf("dbtest: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Logf("dbtest: cleanup connect: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("dbtest: drop %s: %v", name, err)
		}
	})
	return pool
}

// Migrated returns a fresh database with every migration applied. A test that has to change the
// schema or touch what only the owner may (a trigger, TRUNCATE) uses MigratedAsOwner.
func Migrated(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool := MigratedAsOwner(t)
	if os.Getenv(RuntimeRoleEnvVar) != "" {
		return asRuntimeRole(t, pool)
	}
	return pool
}

// MigratedAsOwner is Migrated, always connected as the test administrator, which owns the schema.
func MigratedAsOwner(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool := New(t)
	m, err := db.NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("dbtest: migrate: %v", err)
	}
	return pool
}

// RuntimeRoleEnvVar names the variable that, when set, makes Migrated hand out a pool that connects as the controller's limited
// runtime role (table rights only, docs/deployment.md "Database roles") instead of as the test administrator:
// the whole suite then proves the controller needs nothing more. The test that needs more (a schema change) uses New.
const RuntimeRoleEnvVar = "OPENDECTCELL_TEST_RUNTIME_ROLE"

// owners maps a pool that connects as the runtime role to the owner's pool on the same database.
var owners sync.Map

// Owner is the connection of the schema's owner to the database of a pool Migrated returned: the pool itself, or, when the
// suite runs as the runtime role (RuntimeRoleEnvVar), the administrator's. For the few steps of a test that only the owner may do.
func Owner(pool *pgxpool.Pool) *pgxpool.Pool {
	if a, ok := owners.Load(pool); ok {
		return a.(*pgxpool.Pool)
	}
	return pool
}

func asRuntimeRole(t testing.TB, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	var b [6]byte
	rand.Read(b[:])
	role := "odc_app_" + hex.EncodeToString(b[:])
	pw := "pw-" + hex.EncodeToString(b[:])
	if err := db.EnsureRuntimeRole(ctx, admin, role, pw); err != nil {
		t.Fatalf("dbtest: runtime role: %v", err)
	}
	if err := db.GrantRuntime(ctx, admin, role); err != nil {
		t.Fatalf("dbtest: grants: %v", err)
	}
	c := admin.Config().ConnConfig
	u := url.URL{Scheme: "postgres", User: url.UserPassword(role, pw), Host: net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port))), Path: "/" + c.Database, RawQuery: "sslmode=disable"}
	rt, err := db.Open(ctx, u.String(), 5)
	if err != nil {
		t.Fatalf("dbtest: connect as the runtime role: %v", err)
	}
	owners.Store(rt, admin)
	t.Cleanup(func() {
		owners.Delete(rt)
		rt.Close()
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.Exec(cctx, "DROP OWNED BY "+pgx.Identifier{role}.Sanitize())
		_, _ = admin.Exec(cctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
	})
	return rt
}
