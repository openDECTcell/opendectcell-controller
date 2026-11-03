// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// runLockKey is the advisory lock a running controller holds ("openDECTcell" in ASCII). It exists so that a command that must not
// run beside a live controller (rotate-dek: the controller keeps the old keys in memory and would seal new secrets under a
// key the rotation deletes) can tell. The lock belongs to a database connection, so it goes with the process.
const runLockKey int64 = 0x6465637477727400

// ErrControllerRunning is returned by CheckNoController when a controller holds the run lock.
var ErrControllerRunning = errors.New("db: a controller is running against this database; stop it first")

// HoldRunLock takes the run lock on a connection of its own and keeps it until release is called. held is false when
// another controller has it (two controllers on one database is a mistake the caller may log); the caller then has no
// release to call.
func HoldRunLock(ctx context.Context, pool *pgxpool.Pool) (release func(), held bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", runLockKey).Scan(&got); err != nil || !got {
		conn.Release()
		return nil, false, err
	}
	return func() {
		// unlock, then hand the connection back; if the unlock fails the connection is closed, which frees the lock too
		if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", runLockKey); err != nil {
			_ = conn.Conn().Close(context.Background())
		}
		conn.Release()
	}, true, nil
}

// CheckNoController returns ErrControllerRunning if a controller holds the run lock.
func CheckNoController(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", runLockKey).Scan(&got); err != nil {
		return err
	}
	if !got {
		return ErrControllerRunning
	}
	_, err = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", runLockKey)
	return err
}
