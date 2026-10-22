// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

// EncryptedColumn names a column that holds secret.Keyring ciphertexts. The
// associated data of each value is (Table, Column, primary key as text).
type EncryptedColumn struct {
	Table, Column, Key string
}

// EncryptedColumns lists every encrypted column in the schema. A new secret
// column must be added here, or key rotation will leave it behind; the
// schema test checks that every *_enc column is listed.
var EncryptedColumns = []EncryptedColumn{
	{"handset", "uak_enc", "id"},
	{"sip_account", "password_enc", "id"},
	{"cell_ca", "key_enc", "id"},
}

// ErrWrongMasterKey means a data key could not be unwrapped with the
// configured master key(s).
var ErrWrongMasterKey = errors.New("db: the master key does not match the database (data keys cannot be unwrapped)")

// LoadKeyring unwraps every data key with the master key (or, during a
// master-key rotation, the old one). On an empty database it creates the
// first data key. It fails if any data key cannot be unwrapped, so a wrong
// master key stops the controller at start-up rather than at first use.
func LoadKeyring(ctx context.Context, pool *pgxpool.Pool, mk secret.MasterKey, old *secret.MasterKey) (*secret.Keyring, error) {
	if err := ensureDEK(ctx, pool, mk); err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, "SELECT id, kek_id, wrapped, active FROM data_key ORDER BY id")
	if err != nil {
		return nil, err
	}
	type dk struct {
		id     uint32
		kekID  string
		wr     []byte
		active bool
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dk, error) {
		var k dk
		var id int32
		err := r.Scan(&id, &k.kekID, &k.wr, &k.active)
		k.id = uint32(id)
		return k, err
	})
	if err != nil {
		return nil, err
	}
	kr := secret.NewKeyring()
	for _, k := range keys {
		key := mk
		if k.kekID != mk.ID() {
			if old == nil || k.kekID != old.ID() {
				return nil, fmt.Errorf("%w: data key %d is wrapped by master key %s", ErrWrongMasterKey, k.id, k.kekID)
			}
			key = *old
		}
		dek, err := key.Unwrap(k.id, k.wr)
		if err != nil {
			return nil, fmt.Errorf("%w: data key %d", ErrWrongMasterKey, k.id)
		}
		if err := kr.Add(k.id, dek, k.active); err != nil {
			return nil, err
		}
		clear(dek)
	}
	if _, ok := kr.ActiveID(); !ok {
		return nil, errors.New("db: no active data key")
	}
	return kr, nil
}

// ensureDEK creates the first data key if there is none.
func ensureDEK(ctx context.Context, pool *pgxpool.Pool, mk secret.MasterKey) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "LOCK TABLE data_key IN EXCLUSIVE MODE"); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM data_key").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		_, err := insertDEK(ctx, tx, mk, true)
		return err
	})
}

func insertDEK(ctx context.Context, tx pgx.Tx, mk secret.MasterKey, active bool) (uint32, error) {
	var id int32
	// Reserve the id first: it is bound into the wrapping.
	if err := tx.QueryRow(ctx,
		"INSERT INTO data_key (kek_id, wrapped, active) VALUES ($1, '\\x', false) RETURNING id", mk.ID()).Scan(&id); err != nil {
		return 0, err
	}
	dek, err := secret.NewDEK()
	if err != nil {
		return 0, err
	}
	defer clear(dek)
	w, err := mk.Wrap(uint32(id), dek)
	if err != nil {
		return 0, err
	}
	if active {
		if _, err := tx.Exec(ctx, "UPDATE data_key SET active = false WHERE active"); err != nil {
			return 0, err
		}
	}
	_, err = tx.Exec(ctx, "UPDATE data_key SET wrapped = $2, active = $3 WHERE id = $1", id, w, active)
	return uint32(id), err
}

// Rekey re-wraps every data key under mk; old must unwrap the ones that are
// not yet under mk. It returns the number of keys re-wrapped.
func Rekey(ctx context.Context, pool *pgxpool.Pool, mk, old secret.MasterKey) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT id, wrapped FROM data_key WHERE kek_id <> $1 FOR UPDATE", mk.ID())
		if err != nil {
			return err
		}
		type row struct {
			id int32
			w  []byte
		}
		rs, err := pgx.CollectRows(rows, func(cr pgx.CollectableRow) (row, error) {
			var r row
			return r, cr.Scan(&r.id, &r.w)
		})
		if err != nil {
			return err
		}
		for _, r := range rs {
			dek, err := old.Unwrap(uint32(r.id), r.w)
			if err != nil {
				return fmt.Errorf("%w: data key %d (with the old master key)", ErrWrongMasterKey, r.id)
			}
			w, err := mk.Wrap(uint32(r.id), dek)
			clear(dek)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE data_key SET kek_id = $2, wrapped = $3 WHERE id = $1", r.id, mk.ID(), w); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// RotateDEK makes a new data key active, re-encrypts every secret under it,
// and deletes data keys nothing refers to any more. It returns the number of
// values re-encrypted. kr must hold every current data key; the new key is
// added to it and made active only once the transaction has committed.
func RotateDEK(ctx context.Context, pool *pgxpool.Pool, mk secret.MasterKey, kr *secret.Keyring) (int, error) {
	n := 0
	var newID uint32
	var newDEK []byte
	defer func() { clear(newDEK) }()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "LOCK TABLE data_key IN EXCLUSIVE MODE"); err != nil {
			return err
		}
		id, err := insertDEK(ctx, tx, mk, true)
		if err != nil {
			return err
		}
		var w []byte
		if err := tx.QueryRow(ctx, "SELECT wrapped FROM data_key WHERE id = $1", int32(id)).Scan(&w); err != nil {
			return err
		}
		dek, err := mk.Unwrap(id, w)
		if err != nil {
			return err
		}
		newID, newDEK = id, dek
		// Work on a copy, so a rollback leaves kr untouched.
		work := kr.Clone()
		if err := work.Add(id, dek, true); err != nil {
			return err
		}
		for _, c := range EncryptedColumns {
			q := fmt.Sprintf("SELECT %s::text, %s FROM %s WHERE %s IS NOT NULL FOR UPDATE",
				pgx.Identifier{c.Key}.Sanitize(), pgx.Identifier{c.Column}.Sanitize(),
				pgx.Identifier{c.Table}.Sanitize(), pgx.Identifier{c.Column}.Sanitize())
			rows, err := tx.Query(ctx, q)
			if err != nil {
				return err
			}
			type row struct {
				key string
				ct  []byte
			}
			rs, err := pgx.CollectRows(rows, func(cr pgx.CollectableRow) (row, error) {
				var r row
				return r, cr.Scan(&r.key, &r.ct)
			})
			if err != nil {
				return err
			}
			upd := fmt.Sprintf("UPDATE %s SET %s = $2 WHERE %s::text = $1",
				pgx.Identifier{c.Table}.Sanitize(), pgx.Identifier{c.Column}.Sanitize(), pgx.Identifier{c.Key}.Sanitize())
			for _, r := range rs {
				loc := secret.Location{Table: c.Table, Column: c.Column, RowID: r.key}
				pt, err := work.Open(loc, r.ct)
				if err != nil {
					return fmt.Errorf("%s.%s row %s: %w", c.Table, c.Column, r.key, err)
				}
				ct, err := work.Seal(loc, pt)
				clear(pt)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, upd, r.key, ct); err != nil {
					return err
				}
				n++
			}
		}
		// Every value is now under the new key, so all others are unused.
		_, err = tx.Exec(ctx, "DELETE FROM data_key WHERE id <> $1", int32(id))
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, kr.Add(newID, newDEK, true)
}
