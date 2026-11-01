// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

var ctx = context.Background()

func TestMigrateUpDownUp(t *testing.T) {
	pool := dbtest.New(t)
	m, err := db.NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	did, err := m.Up(ctx)
	if err != nil || len(did) == 0 {
		t.Fatalf("Up: %v %v", did, err)
	}
	cur, latest, err := m.Status(ctx)
	if err != nil || cur != latest {
		t.Fatalf("Status: %d/%d %v", cur, latest, err)
	}
	if again, err := m.Up(ctx); err != nil || len(again) != 0 {
		t.Fatalf("second Up: %v %v", again, err)
	}
	if _, err := m.Down(ctx, latest); err != nil {
		t.Fatalf("Down: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'schema_migration'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tables left after full rollback: %d %v", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'audit_log_guard'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("function left after rollback: %d %v", n, err)
	}
	if _, err := m.Up(ctx); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
}

func TestRefusesEditedOrNewerSchema(t *testing.T) {
	pool := dbtest.Migrated(t)
	m, _ := db.NewMigrator(pool)
	if _, err := pool.Exec(ctx, "UPDATE schema_migration SET checksum = '\\x00' WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Up(ctx); err == nil || !strings.Contains(err.Error(), "changed after it was applied") {
		t.Fatalf("edited migration: %v", err)
	}
	ms, _ := db.Migrations()
	if _, err := pool.Exec(ctx, "UPDATE schema_migration SET checksum = $1 WHERE version = 1", ms[0].Checksum[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO schema_migration (version, name, checksum) VALUES ($1, 'future', '\\x00')", len(ms)+1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Up(ctx); err == nil || !strings.Contains(err.Error(), "newer controller") {
		t.Fatalf("newer schema: %v", err)
	}
}

func TestParseMigrationsRejectsBadFiles(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"bad name": {"m/1_x.sql": {Data: []byte("-- +up\nx\n-- +down\n")}},
		"gap":      {"m/0002_x.sql": {Data: []byte("-- +up\nx\n-- +down\n")}},
		"no down":  {"m/0001_x.sql": {Data: []byte("-- +up\nx\n")}},
		"order":    {"m/0001_x.sql": {Data: []byte("-- +down\ny\n-- +up\nx\n")}},
		"empty up": {"m/0001_x.sql": {Data: []byte("-- +up\n\n-- +down\ny\n")}},
		"two up":   {"m/0001_x.sql": {Data: []byte("-- +up\nx\n-- +up\n-- +down\n")}},
	} {
		if _, err := db.ParseMigrations(fsys, "m"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEveryEncryptedColumnIsRegistered(t *testing.T) {
	pool := dbtest.Migrated(t)
	rows, err := pool.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name LIKE '%\_enc' ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var tbl, col string
		rows.Scan(&tbl, &col)
		found = append(found, tbl+"."+col)
	}
	var reg []string
	for _, c := range db.EncryptedColumns {
		reg = append(reg, c.Table+"."+c.Column)
	}
	slices.Sort(reg)
	if !slices.Equal(found, reg) {
		t.Fatalf("schema has %v, EncryptedColumns has %v", found, reg)
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	pool := dbtest.Migrated(t)
	if _, err := pool.Exec(ctx, "INSERT INTO audit_log (actor_name, action) VALUES ('t', 'test')"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"UPDATE audit_log SET action = 'x'", "DELETE FROM audit_log", "TRUNCATE audit_log"} {
		if _, err := pool.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v", q, err)
		}
	}
	tx, _ := pool.Begin(ctx)
	defer tx.Rollback(ctx)
	tx.Exec(ctx, "SET LOCAL opendectcell.audit_purge = 'on'")
	if tag, err := tx.Exec(ctx, "DELETE FROM audit_log"); err != nil || tag.RowsAffected() != 1 {
		t.Errorf("purge: %v %v", tag, err)
	}
}

func TestLinkSignatureUnique(t *testing.T) {
	pool := dbtest.Migrated(t)
	owner := newUser(t, pool)
	ins := "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, $2, $3, '\\x00')"
	if _, err := pool.Exec(ctx, ins, owner, []byte{1}, 0x01234); err != nil {
		t.Fatal(err)
	}
	// Same low 16 bits, different TPUI: same link signature.
	if _, err := pool.Exec(ctx, ins, owner, []byte{2}, 0x11234); err == nil {
		t.Fatal("two handsets with the same link signature")
	}
}

func newUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, "INSERT INTO app_user (username, role, auth_source) VALUES ('owner', 'user', 'local') RETURNING id::text").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A handset always has an owner (0004).
func TestHandsetOwnerRequired(t *testing.T) {
	pool := dbtest.Migrated(t)
	if _, err := pool.Exec(ctx, "INSERT INTO handset (ipui, tpui, uak_enc) VALUES ('\\x01', 1, '\\x')"); err == nil {
		t.Fatal("a handset without an owner")
	}
}

func masterKey(t *testing.T) secret.MasterKey {
	s, _ := secret.GenerateMasterKey()
	mk, err := secret.ParseMasterKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return mk
}

func TestKeyringLifecycle(t *testing.T) {
	pool := dbtest.Migrated(t)
	mk := masterKey(t)
	kr, err := db.LoadKeyring(ctx, pool, mk, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A secret in each encrypted column.
	var hid, cid, sid string
	loc := func(tbl, col, id string) secret.Location { return secret.Location{Table: tbl, Column: col, RowID: id} }
	pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\\x01', 1, '\\x') RETURNING id::text", newUser(t, pool)).Scan(&hid)
	pool.QueryRow(ctx, "INSERT INTO sip_account (label, username, domain) VALUES ('l', 'u', 'd') RETURNING id::text").Scan(&sid)
	pool.QueryRow(ctx, "INSERT INTO cell_ca (cert_der, key_enc, not_after) VALUES ('\\x', '\\x', now()) RETURNING id::text").Scan(&cid)
	seal := func(tbl, col, id, v string) {
		ct, err := kr.Seal(loc(tbl, col, id), []byte(v))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "UPDATE "+tbl+" SET "+col+" = $2 WHERE id::text = $1", id, ct); err != nil {
			t.Fatal(err)
		}
	}
	seal("handset", "uak_enc", hid, "uak")
	seal("sip_account", "password_enc", sid, "pw")
	seal("sip_account", "stunpass_enc", sid, "turnpw")
	seal("cell_ca", "key_enc", cid, "cakey")

	// Reloading finds the same key; a wrong master key is refused.
	if _, err := db.LoadKeyring(ctx, pool, mk, nil); err != nil {
		t.Fatal(err)
	}
	other := masterKey(t)
	if _, err := db.LoadKeyring(ctx, pool, other, nil); !errors.Is(err, db.ErrWrongMasterKey) {
		t.Fatalf("wrong master key: %v", err)
	}

	// Master-key rotation: loads with old as fallback, rekey, then new alone.
	if _, err := db.LoadKeyring(ctx, pool, other, &mk); err != nil {
		t.Fatalf("load during rotation: %v", err)
	}
	if n, err := db.Rekey(ctx, pool, other, mk); err != nil || n != 1 {
		t.Fatalf("Rekey: %d %v", n, err)
	}
	kr, err = db.LoadKeyring(ctx, pool, other, nil)
	if err != nil {
		t.Fatalf("load after rekey: %v", err)
	}

	// DEK rotation re-encrypts everything and drops the old key.
	before, _ := kr.ActiveID()
	n, err := db.RotateDEK(ctx, pool, other, kr)
	if err != nil || n != 4 {
		t.Fatalf("RotateDEK: %d %v", n, err)
	}
	after, _ := kr.ActiveID()
	if after == before {
		t.Fatal("active DEK unchanged")
	}
	var keys int
	pool.QueryRow(ctx, "SELECT count(*) FROM data_key").Scan(&keys)
	if keys != 1 {
		t.Fatalf("%d data keys left, want 1", keys)
	}
	fresh, err := db.LoadKeyring(ctx, pool, other, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tbl, col, id, want string }{
		{"handset", "uak_enc", hid, "uak"}, {"sip_account", "password_enc", sid, "pw"},
		{"sip_account", "stunpass_enc", sid, "turnpw"}, {"cell_ca", "key_enc", cid, "cakey"},
	} {
		var ct []byte
		pool.QueryRow(ctx, "SELECT "+c.col+" FROM "+c.tbl+" WHERE id::text = $1", c.id).Scan(&ct)
		if id, _ := secret.DEKID(ct); id != after {
			t.Errorf("%s still under DEK %d", c.tbl, id)
		}
		pt, err := fresh.Open(loc(c.tbl, c.col, c.id), ct)
		if err != nil || string(pt) != c.want {
			t.Errorf("%s: %q %v", c.tbl, pt, err)
		}
	}
}
