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
	"net/url"
	"os"
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

// Migrated returns a fresh database with every migration applied.
func Migrated(t testing.TB) *pgxpool.Pool {
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
