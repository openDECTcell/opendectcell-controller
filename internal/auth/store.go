// SPDX-License-Identifier: AGPL-3.0-or-later

// Package auth implements local and OIDC login, server-side sessions,
// login rate limiting and the authorisation helpers every handler uses
// (AGENTS.md §5, docs/threat-model.md B4).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/mail"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

// Role is a user's role.
type Role string

// The two roles. An admin manages users, cells and settings; a user sees and
// changes only their own handsets and accounts (AGENTS.md §5).
const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// User is an authenticated principal.
type User struct {
	ID          uuid.UUID
	Username    string
	DisplayName string
	Email       string
	Role        Role
	MayAddCells bool
	// EnrollOpen: the installation lets every user enroll cells (a setting, set when
	// the user is loaded for a request; not stored per user).
	EnrollOpen  bool
	AuthSource  string // "local" or "oidc"
	Disabled    bool
	CreatedAt   time.Time
	LastLoginAt *time.Time
	// PendingApproval: signed up by themselves, waiting for an admin
	// (disabled until then).
	PendingApproval bool
	// EmailVerified is false for a sign-up that has not followed the link
	// in its e-mail yet (disabled until it does).
	EmailVerified bool
}

// IsAdmin reports whether u has the admin role.
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// CanAddCells reports whether u may enroll cells (see the package function).
func (u *User) CanAddCells() bool { return CanAddCells(u) }

// Name is what the UI shows.
func (u *User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

// Actor returns the audit actor for u.
func (u *User) Actor() audit.Actor {
	id := u.ID
	return audit.Actor{UserID: &id, Name: u.Username}
}

// Errors of the user and session store.
//
//   - ErrNotFound: no such user, or no local password to check or change
//     (OIDC users have none).
//   - ErrBadLogin: unknown user or wrong password, deliberately not told
//     apart, so a login does not reveal which usernames exist.
//   - ErrRateLimited: too many failed attempts for this key within RateWindow.
//   - ErrUsernameTaken: the username is already in use.
//   - ErrNoSession: the cookie names no live session (unknown, expired,
//     idle too long, or its user is disabled).
var (
	ErrNotFound      = errors.New("auth: not found")
	ErrBadLogin      = errors.New("auth: invalid username or password")
	ErrRateLimited   = errors.New("auth: too many failed attempts; try again later")
	ErrUsernameTaken = errors.New("auth: username already exists")
	ErrNoSession     = errors.New("auth: no valid session")
)

// Store holds users and sessions.
type Store struct {
	Pool          *pgxpool.Pool
	Argon         ArgonParams
	SessionIdle   time.Duration
	SessionMaxAge time.Duration
	RateLimit     int           // failures allowed per key per RateWindow
	RateWindow    time.Duration // default 15 min
	Now           func() time.Time
	// Keyring protects two-factor secrets; without it two-factor login is off.
	Keyring *secret.Keyring
	// EveryoneMayEnroll reports the installation's setting that lets every user enroll
	// cells; nil: it does not, and only MayAddCells and admins do.
	EveryoneMayEnroll func(context.Context) bool

	// HashConcurrency bounds the password hashes (Argon2id uses tens of MiB
	// each) that run at once; others wait, for at most hashWait. Zero is
	// min(4, CPUs).
	HashConcurrency int

	dummyOnce sync.Once
	dummyHash string
	semOnce   sync.Once
	sem       chan struct{}
}

// hashWait is how long a request waits for a place to hash a password before
// it is turned away like a rate-limited one.
const hashWait = 10 * time.Second

// hashSlot takes one of the places for an Argon2id run. The returned function
// gives it back.
func (s *Store) hashSlot(ctx context.Context) (func(), error) {
	s.semOnce.Do(func() {
		n := s.HashConcurrency
		if n <= 0 {
			n = min(4, runtime.NumCPU())
		}
		s.sem = make(chan struct{}, n)
	})
	t := time.NewTimer(hashWait)
	defer t.Stop()
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	case <-t.C:
		return nil, ErrRateLimited
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// hash and verify are HashPassword and VerifyPassword behind the limit on
// concurrent runs.
func (s *Store) hash(ctx context.Context, password string) (string, error) {
	done, err := s.hashSlot(ctx)
	if err != nil {
		return "", err
	}
	defer done()
	return HashPassword(s.Argon, password)
}

func (s *Store) verify(ctx context.Context, encoded, password string) (bool, error) {
	done, err := s.hashSlot(ctx)
	if err != nil {
		return false, err
	}
	defer done()
	return VerifyPassword(encoded, password)
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

const userCols = `id, username, display_name, coalesce(email, ''), role, may_add_cells, auth_source, disabled, created_at, last_login_at, pending_approval, email_verified`

func scanUser(r pgx.Row) (*User, error) {
	var u User
	var role string
	err := r.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Email, &role, &u.MayAddCells, &u.AuthSource, &u.Disabled, &u.CreatedAt, &u.LastLoginAt, &u.PendingApproval, &u.EmailVerified)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	u.Role = Role(role)
	return &u, err
}

// UserByID returns a user.
func (s *Store) UserByID(ctx context.Context, q db.Querier, id uuid.UUID) (*User, error) {
	return scanUser(q.QueryRow(ctx, "SELECT "+userCols+" FROM app_user WHERE id = $1", id))
}

// UserByUsername returns a user, matching the name case-insensitively.
func (s *Store) UserByUsername(ctx context.Context, q db.Querier, name string) (*User, error) {
	return scanUser(q.QueryRow(ctx, "SELECT "+userCols+" FROM app_user WHERE lower(username) = lower($1)", name))
}

// NewLocalUser describes a local account to create.
type NewLocalUser struct {
	Username, DisplayName, Email, Password string
	Role                                   Role
	MayAddCells                            bool
}

// ValidUsername applies the username rules shared by local and OIDC users.
func ValidUsername(name string) error {
	if name == "" || len(name) > 128 {
		return errors.New("username must be 1 to 128 characters")
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return errors.New("username must not contain spaces or control characters")
		}
	}
	return nil
}

// CreateLocalUser creates a local account and records it in the audit log,
// in one transaction.
func (s *Store) CreateLocalUser(ctx context.Context, n NewLocalUser) (*User, error) {
	if err := ValidUsername(n.Username); err != nil {
		return nil, err
	}
	if n.Role != RoleAdmin && n.Role != RoleUser {
		return nil, errors.New("role must be admin or user")
	}
	if err := CheckNewPassword(n.Password); err != nil {
		return nil, err
	}
	hash, err := s.hash(ctx, n.Password)
	if err != nil {
		return nil, err
	}
	var u *User
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var email *string
		if n.Email != "" {
			email = &n.Email
		}
		u, err = scanUser(tx.QueryRow(ctx, `INSERT INTO app_user (username, display_name, email, role, may_add_cells, auth_source, password_hash)
			VALUES ($1, $2, $3, $4, $5, 'local', $6) RETURNING `+userCols,
			n.Username, n.DisplayName, email, string(n.Role), n.MayAddCells, hash))
		if isUniqueViolation(err) {
			return ErrUsernameTaken
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.create", ObjectType: "user", ObjectID: u.ID.String(),
			Details: map[string]any{"username": u.Username, "role": string(u.Role), "auth_source": "local"}})
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// VerifyTTL is how long the link in a verification mail works.
const VerifyTTL = 24 * time.Hour

// ErrRegisterRate is returned when one client address signs up too often.
var ErrRegisterRate = errors.New("auth: too many sign-ups from this address; try again later")

// SelfRegistration is a sign-up: a username, an e-mail address and a password.
type SelfRegistration struct {
	Username, Email, Password string
	// Pending creates the account disabled until an admin approves it.
	Pending bool
	// Unverified creates it disabled until the address is verified; the
	// caller then sends the link (NewEmailToken).
	Unverified bool
}

// ValidEmail accepts a plain address, without a display name or comments.
func ValidEmail(email string) error {
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email || len(email) > 254 || a.Name != "" {
		return errors.New("e-mail address is not valid")
	}
	return nil
}

// RegisterSelf creates a user account for someone signing up. It is limited
// per client address (every attempt counts, successful or not), always makes
// a user (never an admin) and records the sign-up in the audit log.
func (s *Store) RegisterSelf(ctx context.Context, n SelfRegistration, addr string) (*User, error) {
	keys := []string{"register:" + addr}
	if addr != "" {
		if _, err := s.reserve(ctx, keys); errors.Is(err, ErrRateLimited) {
			return nil, ErrRegisterRate
		} else if err != nil {
			return nil, err
		}
	}
	if err := ValidUsername(n.Username); err != nil {
		return nil, err
	}
	if err := ValidEmail(n.Email); err != nil {
		return nil, err
	}
	if err := CheckNewPassword(n.Password); err != nil {
		return nil, err
	}
	hash, err := s.hash(ctx, n.Password)
	if err != nil {
		return nil, err
	}
	var u *User
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		u, err = scanUser(tx.QueryRow(ctx, `INSERT INTO app_user (username, email, role, auth_source, password_hash, disabled, pending_approval, email_verified)
			VALUES ($1, $2, 'user', 'local', $3, $4 OR $5, $4, NOT $5) RETURNING `+userCols, n.Username, n.Email, hash, n.Pending, n.Unverified))
		if isUniqueViolation(err) {
			return ErrUsernameTaken
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.register", ObjectType: "user", ObjectID: u.ID.String(),
			Details: map[string]any{"username": u.Username, "pending": n.Pending, "unverified": n.Unverified}})
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// SetPassword replaces a local user's password and ends their other sessions.
// keepSession, if non-nil, is the session to keep (the one changing it).
func (s *Store) SetPassword(ctx context.Context, userID uuid.UUID, password string, keepSession []byte) error {
	if err := CheckNewPassword(password); err != nil {
		return err
	}
	hash, err := s.hash(ctx, password)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE app_user SET password_hash = $2 WHERE id = $1 AND auth_source = 'local'", userID, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, "DELETE FROM session WHERE user_id = $1 AND ($2::bytea IS NULL OR token_hash <> $2)", userID, keepSession); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.password_change", ObjectType: "user", ObjectID: userID.String()})
	})
}

// rate limiting

func (s *Store) window() time.Duration {
	if s.RateWindow > 0 {
		return s.RateWindow
	}
	return 15 * time.Minute
}

// rateKeys are the keys a login attempt counts against: the username and
// the client address, so neither a single name nor a single client can be
// hammered.
func rateKeys(username, addr string) []string {
	keys := []string{"user:" + strings.ToLower(username)}
	if addr != "" {
		keys = append(keys, "addr:"+addr)
	}
	return keys
}

// reserve counts an attempt against the rate limit before the check runs, under
// a lock per key, so a burst of parallel attempts cannot all pass the same
// count. The reserved attempt stays as the failure; call give when the
// attempt succeeded (or ended for a reason that is not the client's fault) to
// take it back.
func (s *Store) reserve(ctx context.Context, keys []string) (give func(), err error) {
	keys = slices.Sorted(slices.Values(keys)) // one lock order, no deadlock
	var ids []int64
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		ids = nil
		for _, k := range keys {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('login_failure'), hashtext($1))", k); err != nil {
				return err
			}
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(c), 0) FROM (
			SELECT count(*) AS c FROM login_failure WHERE key = ANY($1) AND at > $2 GROUP BY key) x`,
			keys, s.now().Add(-s.window())).Scan(&n); err != nil {
			return err
		}
		if n >= s.RateLimit {
			return ErrRateLimited
		}
		rows, err := tx.Query(ctx, "INSERT INTO login_failure (key, at) SELECT unnest($1::text[]), $2 RETURNING id", keys, s.now())
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		return err
	})
	if err != nil {
		return nil, err
	}
	return func() {
		_, _ = s.Pool.Exec(context.WithoutCancel(ctx), "DELETE FROM login_failure WHERE id = ANY($1)", ids)
	}, nil
}

// LoginLocal checks a username and password. It is rate-limited per username
// and per client address, takes the same time for unknown users, and records
// success and failure in the audit log (without the password).
func (s *Store) LoginLocal(ctx context.Context, username, password, addr string) (*User, error) {
	u, err := s.CheckPassword1(ctx, username, password, addr)
	if err != nil {
		return nil, err
	}
	return s.loggedIn(ctx, u, "local")
}

// CheckPassword1 is the first factor of a local login: the same checks as
// LoginLocal (rate limit, equal work for unknown names, audit of failures)
// without recording the login, so a second factor can still follow. It
// returns the user's id.
func (s *Store) CheckPassword1(ctx context.Context, username, password, addr string) (uuid.UUID, error) {
	// The attempt is counted before the password is checked (reserve), so
	// parallel attempts cannot all slip under the limit; a wrong password
	// leaves it counted, a right one or an internal error takes it back.
	give, err := s.reserve(ctx, rateKeys(username, addr))
	if errors.Is(err, ErrRateLimited) {
		// Best effort: the outcome of a login must not depend on the audit write.
		_ = audit.Record(ctx, s.Pool, audit.Entry{Action: "user.login_ratelimited", ObjectType: "user", Details: map[string]any{"username": username}})
		return uuid.Nil, ErrRateLimited
	}
	if err != nil {
		return uuid.Nil, err
	}
	fail := func() (uuid.UUID, error) {
		_ = audit.Record(ctx, s.Pool, audit.Entry{Action: "user.login_failed", ObjectType: "user", Details: map[string]any{"username": username}})
		return uuid.Nil, ErrBadLogin
	}
	if len(password) > MaxPasswordLen {
		return fail()
	}
	var id uuid.UUID
	var hash *string
	var disabled bool
	err = s.Pool.QueryRow(ctx, "SELECT id, password_hash, disabled FROM app_user WHERE lower(username) = lower($1) AND auth_source = 'local'", username).
		Scan(&id, &hash, &disabled)
	if errors.Is(err, pgx.ErrNoRows) || hash == nil {
		// Same work as a real check, so timing doesn't reveal which names exist.
		if _, err := s.verify(ctx, s.dummy(), password); err != nil {
			give()
			return uuid.Nil, err
		}
		return fail()
	}
	if err != nil {
		give()
		return uuid.Nil, err
	}
	ok, err := s.verify(ctx, *hash, password)
	if err != nil {
		give()
		return uuid.Nil, err
	}
	if !ok || disabled {
		return fail()
	}
	give()
	if NeedsRehash(*hash, s.Argon) {
		if h, err := s.hash(ctx, password); err == nil {
			// Best effort: the old hash keeps working if this fails.
			_, _ = s.Pool.Exec(ctx, "UPDATE app_user SET password_hash = $2 WHERE id = $1", id, h)
		}
	}
	return id, nil
}

// CheckPassword re-verifies a logged-in local user's password, e.g. before
// a password change. It is rate-limited like a login but records no login.
func (s *Store) CheckPassword(ctx context.Context, userID uuid.UUID, password, addr string) error {
	var name string
	var hash *string
	err := s.Pool.QueryRow(ctx, "SELECT username, password_hash FROM app_user WHERE id = $1 AND auth_source = 'local'", userID).Scan(&name, &hash)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hash == nil) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	give, err := s.reserve(ctx, rateKeys(name, addr))
	if err != nil {
		return err
	}
	if len(password) <= MaxPasswordLen {
		if ok, err := s.verify(ctx, *hash, password); err != nil {
			give()
			return err
		} else if ok {
			give()
			return nil
		}
	}
	_ = audit.Record(ctx, s.Pool, audit.Entry{Action: "user.password_check_failed", ObjectType: "user", ObjectID: userID.String()})
	return ErrBadLogin
}

// ListUsers returns every user, by username.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+userCols+" FROM app_user ORDER BY lower(username)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) dummy() string {
	s.dummyOnce.Do(func() { s.dummyHash, _ = HashPassword(s.Argon, "dummy password for timing") })
	return s.dummyHash
}

func (s *Store) loggedIn(ctx context.Context, id uuid.UUID, method string) (*User, error) {
	var u *User
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(ctx, "UPDATE app_user SET last_login_at = $2 WHERE id = $1 RETURNING "+userCols, id, s.now()))
		if err != nil {
			return err
		}
		a := audit.ActorFrom(ctx)
		a.UserID, a.Name = &u.ID, u.Username
		return audit.RecordAs(ctx, tx, a, audit.Entry{Action: "user.login", ObjectType: "user", ObjectID: u.ID.String(),
			Details: map[string]any{"method": method}})
	})
	return u, err
}

// sessions

// Session is a validated browser session.
type Session struct {
	TokenHash []byte
	User      *User
	Method    string
	CreatedAt time.Time
}

// NewToken returns 256 random bits, base64url-encoded.
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashToken is how tokens are stored.
func HashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// CreateSession starts a session for u and returns its cookie token.
func (s *Store) CreateSession(ctx context.Context, u *User, method string) (string, error) {
	tok, err := NewToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	_, err = s.Pool.Exec(ctx, "INSERT INTO session (token_hash, user_id, auth_method, created_at, last_seen_at, expires_at) VALUES ($1, $2, $3, $4, $4, $5)",
		HashToken(tok), u.ID, method, now, now.Add(s.SessionMaxAge))
	return tok, err
}

// touchInterval limits how often a session's last_seen_at is written.
const touchInterval = time.Minute

// LookupSession validates a cookie token: the session exists, is within its
// idle and absolute lifetime, and its user is not disabled.
func (s *Store) LookupSession(ctx context.Context, tok string) (*Session, error) {
	if tok == "" || len(tok) > 64 {
		return nil, ErrNoSession
	}
	h := HashToken(tok)
	now := s.now()
	var sess Session
	var lastSeen, expires time.Time
	row := s.Pool.QueryRow(ctx, `SELECT s.auth_method, s.created_at, s.last_seen_at, s.expires_at,
			u.id, u.username, u.display_name, coalesce(u.email, ''), u.role, u.may_add_cells, u.auth_source, u.disabled, u.created_at, u.last_login_at, u.pending_approval, u.email_verified
		FROM session s JOIN app_user u ON u.id = s.user_id WHERE s.token_hash = $1`, h)
	var u User
	var role string
	err := row.Scan(&sess.Method, &sess.CreatedAt, &lastSeen, &expires,
		&u.ID, &u.Username, &u.DisplayName, &u.Email, &role, &u.MayAddCells, &u.AuthSource, &u.Disabled, &u.CreatedAt, &u.LastLoginAt, &u.PendingApproval, &u.EmailVerified)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	u.Role = Role(role)
	if s.EveryoneMayEnroll != nil {
		u.EnrollOpen = s.EveryoneMayEnroll(ctx)
	}
	if u.Disabled || !now.Before(expires) || now.Sub(lastSeen) >= s.SessionIdle {
		_, _ = s.Pool.Exec(ctx, "DELETE FROM session WHERE token_hash = $1", h) // Cleanup retries
		return nil, ErrNoSession
	}
	if now.Sub(lastSeen) >= touchInterval {
		_, _ = s.Pool.Exec(ctx, "UPDATE session SET last_seen_at = $2 WHERE token_hash = $1", h, now) // next request retries
	}
	sess.TokenHash, sess.User = h, &u
	return &sess, nil
}

// DeleteSession ends one session (logout).
func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.Pool.Exec(ctx, "DELETE FROM session WHERE token_hash = $1", tokenHash)
	return err
}

// Cleanup deletes expired sessions, stale rate-limit entries and abandoned
// OIDC logins. Run it periodically.
func (s *Store) Cleanup(ctx context.Context) error {
	now := s.now()
	_, err := s.Pool.Exec(ctx, `
		DELETE FROM session WHERE expires_at <= $1 OR last_seen_at <= $2;`, now, now.Add(-s.SessionIdle))
	if err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, "DELETE FROM login_failure WHERE at <= $1", now.Add(-s.window())); err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, "DELETE FROM oidc_login WHERE expires_at <= $1", now); err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, "DELETE FROM email_token WHERE expires_at <= $1", now.Add(-24*time.Hour)); err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, "DELETE FROM mfa_challenge WHERE expires_at <= $1", now); err != nil {
		return err
	}
	// A sign-up that never followed its link is not kept (data minimisation).
	_, err = s.Pool.Exec(ctx, "DELETE FROM app_user WHERE NOT email_verified AND created_at <= $1", now.Add(-VerifyTTL-24*time.Hour))
	return err
}

// UserUpdate holds the editable fields; nil leaves a field unchanged.
type UserUpdate struct {
	DisplayName, Email *string
	Role               *Role
	MayAddCells        *bool
	Disabled           *bool
}

// ErrSelf means an admin tried to demote, disable or delete themself.
var ErrSelf = errors.New("auth: you cannot do that to your own account")

// ErrHasObjects means the user still owns handsets or SIP accounts.
var ErrHasObjects = errors.New("auth: the user still owns handsets or SIP accounts")

// UpdateUser changes a user (admins only). Disabling ends the user's
// sessions. For OIDC users the role is recomputed at their next login.
func (s *Store) UpdateUser(ctx context.Context, admin *User, id uuid.UUID, up UserUpdate) (*User, error) {
	if RequireAdmin(admin) != nil {
		return nil, ErrForbidden
	}
	if id == admin.ID && ((up.Role != nil && *up.Role != RoleAdmin) || (up.Disabled != nil && *up.Disabled)) {
		return nil, ErrSelf
	}
	if up.Role != nil && *up.Role != RoleAdmin && *up.Role != RoleUser {
		return nil, errors.New("role must be admin or user")
	}
	var u *User
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		old, err := scanUser(tx.QueryRow(ctx, "SELECT "+userCols+" FROM app_user WHERE id = $1 FOR UPDATE", id))
		if err != nil {
			return err
		}
		var role *string
		if up.Role != nil {
			r := string(*up.Role)
			role = &r
		}
		u, err = scanUser(tx.QueryRow(ctx, `UPDATE app_user SET display_name = coalesce($2, display_name),
			email = CASE WHEN $3::text IS NULL THEN email ELSE nullif($3, '') END, role = coalesce($4, role),
			may_add_cells = coalesce($5, may_add_cells), disabled = coalesce($6, disabled),
			pending_approval = CASE WHEN $6::boolean IS NOT NULL AND NOT $6::boolean THEN false ELSE pending_approval END WHERE id = $1 RETURNING `+userCols,
			id, up.DisplayName, up.Email, role, up.MayAddCells, up.Disabled))
		if err != nil {
			return err
		}
		if u.Disabled && !old.Disabled {
			if _, err := tx.Exec(ctx, "DELETE FROM session WHERE user_id = $1", id); err != nil {
				return err
			}
		}
		var fields []string
		d := map[string]any{}
		if old.Role != u.Role {
			d["role_from"], d["role_to"] = string(old.Role), string(u.Role)
		}
		if old.Disabled != u.Disabled {
			d["disabled"] = u.Disabled
		}
		if old.MayAddCells != u.MayAddCells {
			d["may_add_cells"] = u.MayAddCells
		}
		if old.DisplayName != u.DisplayName {
			fields = append(fields, "display_name")
		}
		if old.Email != u.Email {
			fields = append(fields, "email")
		}
		if len(fields) > 0 {
			d["fields"] = fields
		}
		if len(d) == 0 {
			return nil
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.update", ObjectType: "user", ObjectID: id.String(), Details: d})
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// DeleteUser removes a user (admins only) who owns no handsets or SIP
// accounts; their sessions go with them.
func (s *Store) DeleteUser(ctx context.Context, admin *User, id uuid.UUID) error {
	if RequireAdmin(admin) != nil {
		return ErrForbidden
	}
	if id == admin.ID {
		return ErrSelf
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(ctx, "SELECT username FROM app_user WHERE id = $1 FOR UPDATE", id).Scan(&name); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var owns bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM handset WHERE owner_user_id = $1)
			OR EXISTS (SELECT 1 FROM sip_account WHERE owner_user_id = $1)`, id).Scan(&owns); err != nil {
			return err
		}
		if owns {
			return ErrHasObjects
		}
		if _, err := tx.Exec(ctx, "DELETE FROM app_user WHERE id = $1", id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.delete", ObjectType: "user", ObjectID: id.String(), Details: map[string]any{"username": name}})
	})
}
