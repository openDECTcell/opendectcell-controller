// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: RFC 6238 specifies HMAC-SHA1 for the widely supported default
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

// TOTPPeriod is the step of a time-based code in seconds.
const TOTPPeriod = 30

// ErrBadCode is returned for a wrong second-factor code or recovery code.
var ErrBadCode = errors.New("auth: the code is wrong")

// ErrNoKeyring is returned when two-factor login is used on a store that
// has no data key to protect the secret with.
var ErrNoKeyring = errors.New("auth: no keyring for two-factor secrets")

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// totpCode is the 6-digit code of RFC 4226/6238 for a secret and a step.
func totpCode(key []byte, step uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], step)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", n)
}

// matchTOTP returns the step a code belongs to within one step either side
// of now, or 0.
func matchTOTP(key []byte, code string, now time.Time) uint64 {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return 0
	}
	cur := uint64(now.Unix() / TOTPPeriod)
	for _, st := range []uint64{cur, cur - 1, cur + 1} {
		if subtle.ConstantTimeCompare([]byte(totpCode(key, st)), []byte(code)) == 1 {
			return st
		}
	}
	return 0
}

// TOTPCode is the code an authenticator shows for a base32 secret at time t.
// The server never needs it; tests and tools that play the authenticator do.
func TOTPCode(secretB32 string, t time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secretB32, " ", "")))
	if err != nil {
		return "", err
	}
	return totpCode(key, uint64(t.Unix()/TOTPPeriod)), nil
}

// TOTPURI is the otpauth address an authenticator app takes.
func TOTPURI(issuer, account, secretB32 string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secretB32}, "issuer": {issuer}, "period": {strconv.Itoa(TOTPPeriod)}, "digits": {"6"}, "algorithm": {"SHA1"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func totpLoc(id uuid.UUID) secret.Location {
	return secret.Location{Table: "user_totp", Column: "secret_enc", RowID: id.String()}
}

// HasTOTP reports whether a user has confirmed two-factor login.
func (s *Store) HasTOTP(ctx context.Context, id uuid.UUID) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM user_totp WHERE user_id = $1 AND confirmed_at IS NOT NULL)", id).Scan(&ok)
	return ok, err
}

// BeginTOTP makes a new secret for a user and stores it unconfirmed, replacing
// an earlier unconfirmed one. It refuses when two-factor login is already on.
// It returns the secret in base32, shown once for the authenticator.
func (s *Store) BeginTOTP(ctx context.Context, id uuid.UUID) (string, error) {
	if s.Keyring == nil {
		return "", ErrNoKeyring
	}
	if on, err := s.HasTOTP(ctx, id); err != nil || on {
		if err != nil {
			return "", err
		}
		return "", errors.New("two-factor login is already on")
	}
	key := make([]byte, 20)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	sealed, err := s.Keyring.Seal(totpLoc(id), key)
	if err != nil {
		return "", err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO user_totp (user_id, secret_enc) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET secret_enc = EXCLUDED.secret_enc, created_at = now(), last_step = 0 WHERE user_totp.confirmed_at IS NULL`, id, sealed)
	return b32.EncodeToString(key), err
}

func (s *Store) openSecret(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, id uuid.UUID) (key []byte, confirmed bool, last uint64, err error) {
	var sealed []byte
	var conf *time.Time
	var ls int64
	err = q.QueryRow(ctx, "SELECT secret_enc, confirmed_at, last_step FROM user_totp WHERE user_id = $1", id).Scan(&sealed, &conf, &ls)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, 0, ErrBadCode
	}
	if err != nil {
		return nil, false, 0, err
	}
	key, err = s.Keyring.Open(totpLoc(id), sealed)
	return key, conf != nil, uint64(ls), err
}

// RecoveryCodes is how many one-time codes a user gets.
const RecoveryCodes = 10

func newRecoveryCode() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	c := strings.ToLower(b32.EncodeToString(b[:])[:10]) // 50 bits
	return c[:5] + "-" + c[5:], nil
}

func recoveryHash(code string) []byte {
	return HashToken(strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), "-", "")))
}

// ConfirmTOTP turns two-factor login on when the user proves the app works by
// giving a current code. It returns the recovery codes, shown once.
func (s *Store) ConfirmTOTP(ctx context.Context, id uuid.UUID, code string) ([]string, error) {
	if s.Keyring == nil {
		return nil, ErrNoKeyring
	}
	var codes []string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		key, confirmed, _, err := s.openSecret(ctx, tx, id)
		if err != nil {
			return err
		}
		if confirmed {
			return errors.New("two-factor login is already on")
		}
		step := matchTOTP(key, code, s.now())
		if step == 0 {
			return ErrBadCode
		}
		if _, err := tx.Exec(ctx, "UPDATE user_totp SET confirmed_at = $2, last_step = $3 WHERE user_id = $1", id, s.now(), int64(step)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM user_recovery_code WHERE user_id = $1", id); err != nil {
			return err
		}
		for range RecoveryCodes {
			c, err := newRecoveryCode()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO user_recovery_code (user_id, code_hash) VALUES ($1, $2)", id, recoveryHash(c)); err != nil {
				return err
			}
			codes = append(codes, c)
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.totp_on", ObjectType: "user", ObjectID: id.String()})
	})
	return codes, err
}

// VerifySecondFactor checks a TOTP code or a recovery code for a user at
// login. Failures count against the user and the client address. A TOTP step
// can be used once, and a recovery code once.
func (s *Store) VerifySecondFactor(ctx context.Context, id uuid.UUID, code, addr string) error {
	if s.Keyring == nil {
		return ErrNoKeyring
	}
	keys := []string{"totp:" + id.String()}
	if addr != "" {
		keys = append(keys, "addr:"+addr)
	}
	if lim, err := s.limited(ctx, keys); err != nil {
		return err
	} else if lim {
		return ErrRateLimited
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		key, confirmed, last, err := s.openSecret(ctx, tx, id)
		if err != nil {
			return err
		}
		if !confirmed {
			return ErrBadCode
		}
		if step := matchTOTP(key, code, s.now()); step != 0 && step > last {
			_, err := tx.Exec(ctx, "UPDATE user_totp SET last_step = $2 WHERE user_id = $1", id, int64(step))
			return err
		}
		tag, err := tx.Exec(ctx, "UPDATE user_recovery_code SET used_at = $3 WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL", id, recoveryHash(code), s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrBadCode
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.recovery_code_used", ObjectType: "user", ObjectID: id.String()})
	})
	if errors.Is(err, ErrBadCode) {
		if rerr := s.recordFailure(ctx, keys); rerr != nil {
			return rerr
		}
		_ = audit.Record(ctx, s.Pool, audit.Entry{Action: "user.totp_failed", ObjectType: "user", ObjectID: id.String()})
	}
	return err
}

// RecoveryLeft is how many recovery codes a user has not used.
func (s *Store) RecoveryLeft(ctx context.Context, id uuid.UUID) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM user_recovery_code WHERE user_id = $1 AND used_at IS NULL", id).Scan(&n)
	return n, err
}

// DisableTOTP turns two-factor login off for a user and forgets the secret and
// the recovery codes. The caller has checked the password.
func (s *Store) DisableTOTP(ctx context.Context, id uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM user_recovery_code WHERE user_id = $1", id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM user_totp WHERE user_id = $1", id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.totp_off", ObjectType: "user", ObjectID: id.String()})
	})
}

// MFAChallenge is the step between the password and the second factor.
const mfaTTL = 5 * time.Minute

// NewMFAChallenge makes a token that proves the password was right, valid for
// five minutes and for the second-factor step only.
func (s *Store) NewMFAChallenge(ctx context.Context, userID uuid.UUID, returnTo string) (string, error) {
	tok, err := NewToken()
	if err != nil {
		return "", err
	}
	_, err = s.Pool.Exec(ctx, "INSERT INTO mfa_challenge (token_hash, user_id, return_to, expires_at) VALUES ($1, $2, $3, $4)",
		HashToken(tok), userID, returnTo, s.now().Add(mfaTTL))
	return tok, err
}

// PeekMFAChallenge says whose challenge it is and where to go afterwards,
// without using it (a wrong code may be retried, within the rate limit).
func (s *Store) PeekMFAChallenge(ctx context.Context, tok string) (uuid.UUID, string, error) {
	var id uuid.UUID
	var ret string
	err := s.Pool.QueryRow(ctx, "SELECT user_id, return_to FROM mfa_challenge WHERE token_hash = $1 AND expires_at > $2", HashToken(tok), s.now()).Scan(&id, &ret)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, "", ErrBadToken
	}
	return id, ret, err
}

// DropMFAChallenge removes a challenge once the login is complete.
func (s *Store) DropMFAChallenge(ctx context.Context, tok string) error {
	_, err := s.Pool.Exec(ctx, "DELETE FROM mfa_challenge WHERE token_hash = $1", HashToken(tok))
	return err
}

// CompleteLogin records a login whose factors were all checked and returns the user.
func (s *Store) CompleteLogin(ctx context.Context, id uuid.UUID, method string) (*User, error) {
	return s.loggedIn(ctx, id, method)
}
