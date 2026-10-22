// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one numbered schema change with its rollback.
type Migration struct {
	Version  int
	Name     string
	Up, Down string
	Checksum [32]byte // over Up only: editing a rollback is harmless
}

// AppliedMigration is a row of schema_migration.
type AppliedMigration struct {
	Version  int
	Name     string
	Checksum []byte
}

// migrationLockID is the pg_advisory_lock key that serialises migrations
// across controller instances. Arbitrary, but fixed forever.
const migrationLockID = 0x64656374 // "dect"

var fileRE = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// Migrations returns the embedded migrations in version order.
func Migrations() ([]Migration, error) { return parseMigrations(migrationFS, "migrations") }

func parseMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var ms []Migration
	seen := map[int]string{}
	for _, e := range entries {
		m := fileRE.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrations: bad file name %q (want NNNN_name.sql)", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations: version %d used by %s and %s", v, prev, e.Name())
		}
		seen[v] = e.Name()
		b, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		up, down, err := splitSections(string(b))
		if err != nil {
			return nil, fmt.Errorf("migrations: %s: %w", e.Name(), err)
		}
		ms = append(ms, Migration{Version: v, Name: m[2], Up: up, Down: down, Checksum: sha256.Sum256([]byte(up))})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
	for i, m := range ms {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migrations: versions must be 1..n without gaps; found %d at position %d", m.Version, i+1)
		}
	}
	return ms, nil
}

// splitSections splits a file at its "-- +up" and "-- +down" marker lines.
func splitSections(s string) (up, down string, err error) {
	var cur *strings.Builder
	var u, d strings.Builder
	var nUp, nDown int
	for _, line := range strings.SplitAfter(s, "\n") {
		switch strings.TrimSpace(line) {
		case "-- +up":
			cur, nUp = &u, nUp+1
			continue
		case "-- +down":
			cur, nDown = &d, nDown+1
			continue
		}
		if cur != nil {
			cur.WriteString(line)
		}
	}
	if nUp != 1 || nDown != 1 {
		return "", "", errors.New("need exactly one -- +up and one -- +down marker")
	}
	if strings.Index(s, "-- +up") > strings.Index(s, "-- +down") {
		return "", "", errors.New("-- +up must come before -- +down")
	}
	if strings.TrimSpace(u.String()) == "" {
		return "", "", errors.New("empty up section")
	}
	return u.String(), d.String(), nil
}

const createMigrationTable = `
CREATE TABLE IF NOT EXISTS schema_migration (
    version     integer PRIMARY KEY,
    name        text NOT NULL,
    checksum    bytea NOT NULL,
    applied_at  timestamptz NOT NULL DEFAULT now()
)`

// Migrator applies and rolls back the embedded migrations.
type Migrator struct {
	pool *pgxpool.Pool
	ms   []Migration
}

// NewMigrator returns a migrator over the embedded migrations.
func NewMigrator(pool *pgxpool.Pool) (*Migrator, error) {
	ms, err := Migrations()
	if err != nil {
		return nil, err
	}
	return &Migrator{pool: pool, ms: ms}, nil
}

// withLock runs fn on one connection holding the migration advisory lock.
func (m *Migrator) withLock(ctx context.Context, fn func(*pgx.Conn) error) error {
	c, err := m.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer c.Release()
	conn := c.Conn()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockID)
	if _, err := conn.Exec(ctx, createMigrationTable); err != nil {
		return err
	}
	return fn(conn)
}

func applied(ctx context.Context, conn *pgx.Conn) ([]AppliedMigration, error) {
	rows, err := conn.Query(ctx, "SELECT version, name, checksum FROM schema_migration ORDER BY version")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AppliedMigration, error) {
		var a AppliedMigration
		err := r.Scan(&a.Version, &a.Name, &a.Checksum)
		return a, err
	})
}

// check compares what the database has applied with what this binary knows.
// It refuses a database that is ahead of the binary or whose applied
// migrations were edited afterwards.
func (m *Migrator) check(done []AppliedMigration) error {
	for i, a := range done {
		if a.Version != i+1 {
			return fmt.Errorf("schema_migration is not contiguous at version %d", a.Version)
		}
		if a.Version > len(m.ms) {
			return fmt.Errorf("database is at schema version %d but this controller only knows %d: run a newer controller", done[len(done)-1].Version, len(m.ms))
		}
		k := m.ms[a.Version-1]
		if string(a.Checksum) != string(k.Checksum[:]) {
			return fmt.Errorf("migration %04d_%s was changed after it was applied; add a new migration instead", k.Version, k.Name)
		}
	}
	return nil
}

// Up applies every pending migration, each in its own transaction, and
// returns the versions it applied.
func (m *Migrator) Up(ctx context.Context) ([]int, error) {
	var did []int
	err := m.withLock(ctx, func(conn *pgx.Conn) error {
		done, err := applied(ctx, conn)
		if err != nil {
			return err
		}
		if err := m.check(done); err != nil {
			return err
		}
		for _, mg := range m.ms[len(done):] {
			err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, mg.Up); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, "INSERT INTO schema_migration (version, name, checksum) VALUES ($1, $2, $3)",
					mg.Version, mg.Name, mg.Checksum[:])
				return err
			})
			if err != nil {
				return fmt.Errorf("migration %04d_%s: %w", mg.Version, mg.Name, err)
			}
			did = append(did, mg.Version)
		}
		return nil
	})
	return did, err
}

// Down rolls back the newest n applied migrations and returns their versions.
func (m *Migrator) Down(ctx context.Context, n int) ([]int, error) {
	var did []int
	err := m.withLock(ctx, func(conn *pgx.Conn) error {
		done, err := applied(ctx, conn)
		if err != nil {
			return err
		}
		if err := m.check(done); err != nil {
			return err
		}
		for i := len(done) - 1; i >= 0 && len(did) < n; i-- {
			mg := m.ms[done[i].Version-1]
			err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, mg.Down); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, "DELETE FROM schema_migration WHERE version = $1", mg.Version)
				return err
			})
			if err != nil {
				return fmt.Errorf("rollback %04d_%s: %w", mg.Version, mg.Name, err)
			}
			did = append(did, mg.Version)
		}
		return nil
	})
	return did, err
}

// Status returns the applied version and the newest version this binary has.
func (m *Migrator) Status(ctx context.Context) (current, latest int, err error) {
	err = m.withLock(ctx, func(conn *pgx.Conn) error {
		done, err := applied(ctx, conn)
		if err != nil {
			return err
		}
		current = len(done)
		return m.check(done)
	})
	return current, len(m.ms), err
}

// ParseMigrations parses migrations from any file system; exported for tests.
func ParseMigrations(fsys fs.FS, dir string) ([]Migration, error) { return parseMigrations(fsys, dir) }
