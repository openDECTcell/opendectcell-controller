// SPDX-License-Identifier: AGPL-3.0-or-later

// Package apitoken is the personal API tokens (ideas F1): a script or a widget acts as the user who made the token, with the scopes
// the user gave it, until it expires or is revoked. A token is "odc_", 16 hexadecimal characters to look it up by, "_" and 43 characters
// of secret. Only a hash of the secret is stored; the token is shown once, when it is made. A token never gets more than its scopes
// and can never do what only a session may (administration, enrolling base stations, making tokens).
package apitoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
)

// Prefix starts every token, so that a leaked one can be recognised (by secret scanners too).
const Prefix = "odc_"

// The limits of a token.
const (
	MaxPerUser  = 20
	MinLife     = 24 * time.Hour
	MaxLife     = 366 * 24 * time.Hour
	DefaultLife = 90 * 24 * time.Hour
)

// ScopeInfo describes a scope for the page that makes tokens. Scopes are "resource:action", the way most services write them: read
// never changes anything; write is what changes, and is a separate choice.
type ScopeInfo struct {
	Name  string
	Title string
	Write bool
}

// Scopes are all the scopes there are, in the order the page shows them. There is no scope for administration: the administrators' API,
// the base stations' enrolment, and the making of tokens need a session.
var Scopes = []ScopeInfo{
	{"profile:read", "Read who I am", false},
	{"handsets:read", "Read my handsets", false},
	{"handsets:write", "Change my handsets: name, ring policy, lines", true},
	{"lines:read", "Read my VoIP lines and whether they are registered (never their passwords)", false},
	{"lines:write", "Make, change and delete my VoIP lines (including their passwords)", true},
	{"contacts:read", "Read my contacts", false},
	{"contacts:write", "Make, change and delete my contacts", true},
	{"calls:read", "Read my calls", false},
	{"calls:write", "Mark my missed calls as read", true},
	{"statistics:read", "Read my statistics", false},
	{"stations:read", "Read the base stations I may see", false},
	{"registration:write", "Open a registration window to register a handset", true},
}

// ValidScope says whether s is a scope there is.
func ValidScope(s string) bool {
	return slices.ContainsFunc(Scopes, func(i ScopeInfo) bool { return i.Name == s })
}

// ErrInvalid is returned for every token that does not authenticate (malformed, unknown, wrong secret, revoked, expired): the
// caller must not tell which.
var ErrInvalid = errors.New("apitoken: invalid token")

// ErrNotFound means no such token of the user.
var ErrNotFound = errors.New("apitoken: not found")

// ValidationError is a refused input, with a message for the person.
type ValidationError string

func (v ValidationError) Error() string { return string(v) }

// Record is a token as the list shows it. It never holds the secret.
type Record struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Name       string
	Lookup     string
	Scopes     []string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt *time.Time
	LastUsedIP string
	RevokedAt  *time.Time
}

// Display is what the holder recognises a token by: its beginning, which is not a secret.
func (r Record) Display() string { return Prefix + r.Lookup + "_…" }

// Active says whether the token can still be used.
func (r Record) Active(now time.Time) bool { return r.RevokedAt == nil && now.Before(r.ExpiresAt) }

// Token is an authenticated token: whose it is and what it may.
type Token struct {
	ID     uuid.UUID
	UserID uuid.UUID
	Name   string
	Scopes []string
}

// Has says whether the token carries the scope.
func (t Token) Has(scope string) bool { return slices.Contains(t.Scopes, scope) }

// Store keeps the tokens.
type Store struct {
	Pool *pgxpool.Pool
	Now  func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Create makes a token for user. It returns the token itself, which is never available again.
func (s *Store) Create(ctx context.Context, user uuid.UUID, name string, scopes []string, life time.Duration) (string, Record, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", Record{}, ValidationError("Give the token a name of up to 64 characters, so that you know what it is for.")
	}
	if life < MinLife || life > MaxLife {
		return "", Record{}, ValidationError("A token lasts from 1 day to a year.")
	}
	scopes = slices.Clone(scopes)
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	if len(scopes) == 0 {
		return "", Record{}, ValidationError("Choose at least one thing the token may do.")
	}
	for _, sc := range scopes {
		if !ValidScope(sc) {
			return "", Record{}, ValidationError("Unknown scope " + fmt.Sprintf("%q", sc) + ".")
		}
	}
	var lookupRaw [8]byte
	var secretRaw [32]byte
	if _, err := rand.Read(lookupRaw[:]); err != nil {
		return "", Record{}, err
	}
	if _, err := rand.Read(secretRaw[:]); err != nil {
		return "", Record{}, err
	}
	lookup := hex.EncodeToString(lookupRaw[:])
	secret := base64.RawURLEncoding.EncodeToString(secretRaw[:])
	hash := sha256.Sum256([]byte(secret))
	clear(secretRaw[:])
	now := s.now()
	var rec Record
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM api_token WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > $2", user, now).Scan(&n); err != nil {
			return err
		}
		if n >= MaxPerUser {
			return ValidationError(fmt.Sprintf("At most %d tokens at a time: revoke one you do not use.", MaxPerUser))
		}
		rec = Record{UserID: user, Name: name, Lookup: lookup, Scopes: scopes, CreatedAt: now, ExpiresAt: now.Add(life)}
		if err := tx.QueryRow(ctx, `INSERT INTO api_token (user_id, name, lookup, secret_hash, scopes, created_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			user, name, lookup, hash[:], scopes, now, rec.ExpiresAt).Scan(&rec.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "api_token.create", ObjectType: "api_token", ObjectID: rec.ID.String(),
			Details: map[string]any{"label": name, "scopes": strings.Join(scopes, " "), "days": int(life.Hours() / 24)}})
	})
	if err != nil {
		return "", Record{}, err
	}
	return Prefix + lookup + "_" + secret, rec, nil
}

// List returns the user's tokens, newest first, including the revoked and expired ones (the page shows them as such).
func (s *Store) List(ctx context.Context, user uuid.UUID) ([]Record, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, user_id, name, lookup, scopes, created_at, expires_at, last_used_at, coalesce(last_used_ip, ''), revoked_at
		FROM api_token WHERE user_id = $1 ORDER BY created_at DESC, id`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Record, error) {
		var rec Record
		err := r.Scan(&rec.ID, &rec.UserID, &rec.Name, &rec.Lookup, &rec.Scopes, &rec.CreatedAt, &rec.ExpiresAt, &rec.LastUsedAt, &rec.LastUsedIP, &rec.RevokedAt)
		return rec, err
	})
}

// Revoke ends one of the user's tokens at once. Revoking one that is revoked already is not an error.
func (s *Store) Revoke(ctx context.Context, user, id uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(ctx, "SELECT name FROM api_token WHERE id = $1 AND user_id = $2 FOR UPDATE", id, user).Scan(&name); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		tag, err := tx.Exec(ctx, "UPDATE api_token SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL", id, s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "api_token.revoke", ObjectType: "api_token", ObjectID: id.String(), Details: map[string]any{"label": name}})
	})
}

// touchEvery is how often a token's "last used" is written: a script that calls every second does not make a write every second.
const touchEvery = time.Minute

// Authenticate checks a token presented in a request. Every failure is ErrInvalid. addr is the client's address, recorded as the
// last use. The user must still be there and not disabled (the caller loads the user and checks that).
func (s *Store) Authenticate(ctx context.Context, raw, addr string) (Token, error) {
	lookup, secret, ok := parse(raw)
	if !ok {
		return Token{}, ErrInvalid
	}
	var t Token
	var hash []byte
	var expires time.Time
	var revoked *time.Time
	var last *time.Time
	err := s.Pool.QueryRow(ctx, "SELECT id, user_id, name, scopes, secret_hash, expires_at, revoked_at, last_used_at FROM api_token WHERE lookup = $1", lookup).
		Scan(&t.ID, &t.UserID, &t.Name, &t.Scopes, &hash, &expires, &revoked, &last)
	if err != nil {
		// compare against a hash anyway, so that an unknown token does not answer faster than a wrong one
		dummy := sha256.Sum256([]byte(secret))
		subtle.ConstantTimeCompare(dummy[:], make([]byte, sha256.Size))
		if errors.Is(err, pgx.ErrNoRows) {
			return Token{}, ErrInvalid
		}
		return Token{}, err
	}
	got := sha256.Sum256([]byte(secret))
	now := s.now()
	if subtle.ConstantTimeCompare(got[:], hash) != 1 || revoked != nil || !now.Before(expires) {
		return Token{}, ErrInvalid
	}
	if last == nil || now.Sub(*last) >= touchEvery {
		if len(addr) > 64 {
			addr = addr[:64]
		}
		_, _ = s.Pool.Exec(ctx, "UPDATE api_token SET last_used_at = $2, last_used_ip = $3 WHERE id = $1", t.ID, now, addr) // a failed note is not a failed request
	}
	return t, nil
}

// parse splits "odc_" + 16 hexadecimal characters + "_" + 43 characters of secret.
func parse(raw string) (lookup, secret string, ok bool) {
	rest, found := strings.CutPrefix(raw, Prefix)
	if !found || len(rest) != 16+1+43 || rest[16] != '_' {
		return "", "", false
	}
	lookup, secret = rest[:16], rest[17:]
	if _, err := hex.DecodeString(lookup); err != nil {
		return "", "", false
	}
	if _, err := base64.RawURLEncoding.DecodeString(secret); err != nil {
		return "", "", false
	}
	return lookup, secret, true
}
