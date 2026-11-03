// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Database roles (docs/deployment.md, "Database roles"). The controller runs as a role that can read and
// write the tables and nothing else: it cannot change the schema, create roles or read other databases. A
// second, administrative role (in a compose bring-up the one PostgreSQL itself was initialised with) owns the
// schema and is used only by `migrate`.

// RuntimeCredentials reads the user name and the password out of a PostgreSQL connection URL.
func RuntimeCredentials(rawURL string) (user, password string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return "", "", errors.New("db: the database URL names no user")
	}
	pw, _ := u.User.Password()
	return u.User.Username(), pw, nil
}

// IsSuperuser reports whether the pool's role is a PostgreSQL superuser, which the controller should not be.
func IsSuperuser(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var super bool
	err := pool.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super)
	return super, err
}

// EnsureRuntimeRole makes sure a login role with this name and password exists and has no privilege beyond
// logging in: no superuser, no creating databases or roles, no replication. It runs as the administrative role
// and is safe to run again (it also brings the password in line with the URL).
func EnsureRuntimeRole(ctx context.Context, admin *pgxpool.Pool, name, password string) error {
	if name == "" || password == "" {
		return errors.New("db: the runtime role needs a name and a password")
	}
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", name).Scan(&exists); err != nil {
		return err
	}
	var lit string
	if err := admin.QueryRow(ctx, "SELECT quote_literal($1::text)", password).Scan(&lit); err != nil {
		return err
	}
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	_, err := admin.Exec(ctx, verb+" ROLE "+pgx.Identifier{name}.Sanitize()+
		" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD "+lit)
	return err
}

// GrantRuntime lets the role read and write every table and sequence of the public schema and call its
// functions, and nothing else. Run it after the migrations, so it covers what they made.
func GrantRuntime(ctx context.Context, admin *pgxpool.Pool, name string) error {
	r := pgx.Identifier{name}.Sanitize()
	for _, q := range []string{
		"GRANT USAGE ON SCHEMA public TO " + r,
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		"REVOKE CREATE ON SCHEMA public FROM " + r,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO " + r,
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO " + r,
		"GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO " + r,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			return fmt.Errorf("db: %s: %w", q, err)
		}
	}
	return nil
}
