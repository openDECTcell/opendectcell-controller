// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
)

// Purposes of an e-mail token.
const (
	TokenVerify = "verify"
	TokenReset  = "reset"
)

// ErrBadToken is returned for a token that is unknown, used or expired. The
// three are not told apart.
var ErrBadToken = errors.New("auth: the link is invalid or has expired")

// ErrResetRate is returned when one client address asks for resets too often.
var ErrResetRate = errors.New("auth: too many requests from this address; try again later")

// NewEmailToken makes a single-use token for a user and returns it. Only its
// hash is stored. Older unused tokens of the same purpose stop working.
func (s *Store) NewEmailToken(ctx context.Context, userID uuid.UUID, purpose string, ttl time.Duration) (string, error) {
	tok, err := NewToken()
	if err != nil {
		return "", err
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM email_token WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL", userID, purpose); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO email_token (token_hash, user_id, purpose, expires_at) VALUES ($1, $2, $3, $4)",
			HashToken(tok), userID, purpose, s.now().Add(ttl))
		return err
	})
	return tok, err
}

// PeekEmailToken says whose token it is without using it, so a page can ask
// for confirmation first (a mail scanner that opens the link must not
// complete the step).
func (s *Store) PeekEmailToken(ctx context.Context, tok, purpose string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT user_id FROM email_token WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > $3`,
		HashToken(tok), purpose, s.now()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, ErrBadToken
	}
	return id, err
}

// useToken marks a token used inside tx and returns its user.
func (s *Store) useToken(ctx context.Context, tx pgx.Tx, tok, purpose string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `UPDATE email_token SET used_at = $3 WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > $3 RETURNING user_id`,
		HashToken(tok), purpose, s.now()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, ErrBadToken
	}
	return id, err
}

// VerifyEmail completes a sign-up: the address is verified and the account
// opens, unless it still waits for an admin's approval.
func (s *Store) VerifyEmail(ctx context.Context, tok string) (*User, error) {
	var u *User
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		id, err := s.useToken(ctx, tx, tok, TokenVerify)
		if err != nil {
			return err
		}
		u, err = scanUser(tx.QueryRow(ctx, `UPDATE app_user SET email_verified = true, disabled = pending_approval WHERE id = $1 RETURNING `+userCols, id))
		if err != nil {
			return err
		}
		return audit.RecordAs(ctx, tx, u.Actor(), audit.Entry{Action: "user.email_verified", ObjectType: "user", ObjectID: id.String()})
	})
	return u, err
}

// RequestReset finds the local user who may be sent a reset link for email,
// or nil. Every request counts against the client address, whether or not
// the address exists, so the answer does not tell.
func (s *Store) RequestReset(ctx context.Context, email, addr string) (*User, error) {
	keys := []string{"reset:" + addr}
	if addr != "" {
		if lim, err := s.limited(ctx, keys); err != nil {
			return nil, err
		} else if lim {
			return nil, ErrResetRate
		}
		if err := s.recordFailure(ctx, keys); err != nil {
			return nil, err
		}
	}
	u, err := scanUser(s.Pool.QueryRow(ctx, "SELECT "+userCols+" FROM app_user WHERE lower(email) = lower($1) AND auth_source = 'local' AND NOT disabled AND email_verified LIMIT 1", email))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return u, err
}

// ResetPassword sets a new password with a reset token and ends every session
// of the user.
func (s *Store) ResetPassword(ctx context.Context, tok, password string) error {
	if err := CheckNewPassword(password); err != nil {
		return err
	}
	hash, err := HashPassword(s.Argon, password)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		id, err := s.useToken(ctx, tx, tok, TokenReset)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "UPDATE app_user SET password_hash = $2 WHERE id = $1 AND auth_source = 'local' AND NOT disabled", id, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrBadToken
		}
		if _, err := tx.Exec(ctx, "DELETE FROM session WHERE user_id = $1", id); err != nil {
			return err
		}
		return audit.RecordAs(ctx, tx, audit.Actor{UserID: &id, Name: "reset link"}, audit.Entry{Action: "user.password_reset", ObjectType: "user", ObjectID: id.String()})
	})
}

// RequestVerification finds the sign-up waiting for confirmation under email,
// or nil. It counts against the client address like RequestReset.
func (s *Store) RequestVerification(ctx context.Context, email, addr string) (*User, error) {
	keys := []string{"verify:" + addr}
	if addr != "" {
		if lim, err := s.limited(ctx, keys); err != nil {
			return nil, err
		} else if lim {
			return nil, ErrResetRate
		}
		if err := s.recordFailure(ctx, keys); err != nil {
			return nil, err
		}
	}
	u, err := scanUser(s.Pool.QueryRow(ctx, "SELECT "+userCols+" FROM app_user WHERE lower(email) = lower($1) AND NOT email_verified LIMIT 1", email))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return u, err
}
