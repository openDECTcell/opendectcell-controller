// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
)

// SessionInfo is one of a user's sessions as the account page lists it. ID is the first bytes of the stored token hash
// (hex): enough to name a session in a form, useless for logging in.
type SessionInfo struct {
	ID         string
	Device     string
	Method     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	Current    bool
}

const sessionIDBytes = 8

func sessionID(tokenHash []byte) string { return hex.EncodeToString(tokenHash[:sessionIDBytes]) }

// CreateSessionOn is CreateSession that also keeps a short description of the browser.
func (s *Store) CreateSessionOn(ctx context.Context, u *User, method, device string) (string, error) {
	tok, err := s.CreateSession(ctx, u, method)
	if err != nil {
		return "", err
	}
	if device != "" {
		_, err = s.Pool.Exec(ctx, "UPDATE session SET device = $2 WHERE token_hash = $1", HashToken(tok), device)
	}
	return tok, err
}

// ListSessions is the user's live sessions, the one with token hash current marked, the most recently used first.
func (s *Store) ListSessions(ctx context.Context, user uuid.UUID, current []byte) ([]SessionInfo, error) {
	now := s.now()
	rows, err := s.Pool.Query(ctx, `SELECT token_hash, device, auth_method, created_at, last_seen_at FROM session
		WHERE user_id = $1 AND expires_at > $2 AND last_seen_at > $3 ORDER BY last_seen_at DESC`, user, now, now.Add(-s.SessionIdle))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionInfo
	for rows.Next() {
		var h []byte
		var si SessionInfo
		if err := rows.Scan(&h, &si.Device, &si.Method, &si.CreatedAt, &si.LastSeenAt); err != nil {
			return nil, err
		}
		si.ID, si.Current = sessionID(h), string(h) == string(current)
		out = append(out, si)
	}
	return out, rows.Err()
}

// EndSession ends one of the user's sessions by its ID. It never ends the session named by current (that is logout's job)
// and never one of another user. It reports whether a session was ended.
func (s *Store) EndSession(ctx context.Context, user uuid.UUID, id string, current []byte) (bool, error) {
	if len(id) != 2*sessionIDBytes {
		return false, nil
	}
	var ended bool
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM session WHERE user_id = $1 AND encode(substring(token_hash from 1 for $3), 'hex') = $2
			AND token_hash <> $4`, user, id, sessionIDBytes, current)
		if err != nil {
			return err
		}
		if ended = tag.RowsAffected() > 0; !ended {
			return nil
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "session.end", ObjectType: "user", ObjectID: user.String()})
	})
	return ended, err
}

// EndOtherSessions ends every session of the user except current and returns how many it ended. Browsers that were told to
// skip the second step stay trusted: that is ForgetDevices.
func (s *Store) EndOtherSessions(ctx context.Context, user uuid.UUID, current []byte) (int, error) {
	var n int
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "DELETE FROM session WHERE user_id = $1 AND token_hash <> $2", user, current)
		if err != nil {
			return err
		}
		if n = int(tag.RowsAffected()); n == 0 {
			return nil
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "session.end_others", ObjectType: "user", ObjectID: user.String(), Details: map[string]any{"count": n}})
	})
	return n, err
}
