// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
)

// Two-factor login asks for a code on a device it has not seen (no cookie, an unknown or a forgotten one) and on
// a known device again after DeviceTrust. A device is a browser that holds a random token; the database keeps its
// hash, per user, so several users may share one browser.
const (
	DeviceTrust = 21 * 24 * time.Hour // how long a code entered on a device counts
	// deviceForget: a device nobody has logged in from for this long is forgotten altogether.
	deviceForget = 180 * 24 * time.Hour
	// PromptEvery is how long "not now" keeps the offer to set up two-factor login away.
	PromptEvery = 30 * 24 * time.Hour
)

// DeviceTokenOK reports whether tok has the shape of a device token (what NewToken makes), so that garbage
// from a cookie is not looked up or stored.
func DeviceTokenOK(tok string) bool {
	return len(tok) == 43 && strings.Trim(tok, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") == ""
}

// DeviceKnown reports whether the user passed the second step on this device within DeviceTrust. A known device
// is marked as used now.
func (s *Store) DeviceKnown(ctx context.Context, user uuid.UUID, tok string) (bool, error) {
	if !DeviceTokenOK(tok) {
		return false, nil
	}
	now := s.now()
	tag, err := s.Pool.Exec(ctx, `UPDATE trusted_device SET last_seen_at = $3
		WHERE user_id = $1 AND token_hash = $2 AND last_2fa_at > $4`, user, HashToken(tok), now, now.Add(-DeviceTrust))
	return err == nil && tag.RowsAffected() == 1, err
}

// RememberDevice notes that a code was entered on the device: it keeps tok if it is a device token, and returns
// the token to give the browser (a new one if it had none). label says what the device is (the browser's name).
func (s *Store) RememberDevice(ctx context.Context, user uuid.UUID, tok, label string) (string, error) {
	if !DeviceTokenOK(tok) {
		var err error
		if tok, err = NewToken(); err != nil {
			return "", err
		}
	}
	if len(label) > 200 {
		label = label[:200]
	}
	now := s.now()
	_, err := s.Pool.Exec(ctx, `INSERT INTO trusted_device (user_id, token_hash, label, created_at, last_2fa_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $4, $4)
		ON CONFLICT (user_id, token_hash) DO UPDATE SET last_2fa_at = EXCLUDED.last_2fa_at, last_seen_at = EXCLUDED.last_seen_at, label = EXCLUDED.label`,
		user, HashToken(tok), label, now)
	return tok, err
}

// ForgetDevices makes every device of the user unknown again: the next login from each asks for a code.
func (s *Store) ForgetDevices(ctx context.Context, user uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM trusted_device WHERE user_id = $1", user); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.devices_forgotten", ObjectType: "user", ObjectID: user.String()})
	})
}

// DeviceCount is how many devices the user's two-factor login knows.
func (s *Store) DeviceCount(ctx context.Context, user uuid.UUID) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM trusted_device WHERE user_id = $1 AND last_2fa_at > $2", user, s.now().Add(-DeviceTrust)).Scan(&n)
	return n, err
}

// ShouldPromptTOTP reports whether to offer u two-factor login: a local account that has none and has not put the
// offer off in the last PromptEvery. It is an offer, never a demand (administrators' demand is a setting).
func (s *Store) ShouldPromptTOTP(ctx context.Context, u *User) bool {
	if u == nil || u.AuthSource != "local" || s.Keyring == nil {
		return false
	}
	var show bool
	err := s.Pool.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM user_totp WHERE user_id = $1 AND confirmed_at IS NOT NULL)
		AND (twofa_prompt_dismissed_at IS NULL OR twofa_prompt_dismissed_at < $2) FROM app_user WHERE id = $1`,
		u.ID, s.now().Add(-PromptEvery)).Scan(&show)
	return err == nil && show
}

// DismissTOTPPrompt puts the offer off for PromptEvery.
func (s *Store) DismissTOTPPrompt(ctx context.Context, user uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, "UPDATE app_user SET twofa_prompt_dismissed_at = $2 WHERE id = $1", user, s.now())
	if err == nil && tag.RowsAffected() != 1 {
		err = errors.New("auth: no such user")
	}
	return err
}
