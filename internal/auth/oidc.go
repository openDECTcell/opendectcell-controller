// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/config"
)

// ErrNotAuthorized means the IdP authenticated the user, but no group
// mapping grants them a role here.
var ErrNotAuthorized = errors.New("auth: your account is not authorised for this controller")

// ErrOIDCState means the callback's state is unknown or expired.
var ErrOIDCState = errors.New("auth: login session expired or invalid; please try again")

// oidcLoginTTL bounds how long a user may take at the IdP.
const oidcLoginTTL = 10 * time.Minute

// OIDC performs OpenID Connect logins (authorisation code flow with PKCE).
type OIDC struct {
	Store       *Store
	Config      config.OIDC
	RedirectURL string // <base URL>/auth/oidc/callback

	mu       sync.Mutex
	provider *oidc.Provider
}

// provider discovers the IdP on first use and caches it, so the controller
// starts even while the IdP is unreachable.
func (o *OIDC) client(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider == nil {
		p, err := oidc.NewProvider(ctx, o.Config.Issuer)
		if err != nil {
			return nil, nil, fmt.Errorf("auth: OIDC discovery for %s: %w", o.Config.Issuer, err)
		}
		o.provider = p
	}
	return o.provider, &oauth2.Config{
		ClientID:     o.Config.ClientID,
		ClientSecret: o.Config.ClientSecret,
		Endpoint:     o.provider.Endpoint(),
		RedirectURL:  o.RedirectURL,
		Scopes:       o.Config.Scopes,
	}, nil
}

// SafeReturnPath returns p if it is a local absolute path, else "/".
// It blocks open redirects such as "//evil.example" and "/\evil.example".
func SafeReturnPath(p string) string {
	if p == "" || p[0] != '/' || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") || strings.ContainsAny(p, "\r\n") {
		return "/"
	}
	if u, err := url.Parse(p); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return p
}

// Begin starts a login and returns the IdP URL to redirect the browser to, and a token that binds the
// login to that browser: the caller sets it as a cookie and hands it back to Callback. Without it a
// callback URL (state and code) given to another person would sign them in to the attacker's account.
func (o *OIDC) Begin(ctx context.Context, returnTo string) (authURL, bind string, err error) {
	_, oc, err := o.client(ctx)
	if err != nil {
		return "", "", err
	}
	state, err := NewToken()
	if err != nil {
		return "", "", err
	}
	nonce, err := NewToken()
	if err != nil {
		return "", "", err
	}
	if bind, err = NewToken(); err != nil {
		return "", "", err
	}
	verifier := oauth2.GenerateVerifier()
	_, err = o.Store.Pool.Exec(ctx, "INSERT INTO oidc_login (state_hash, browser_hash, nonce, code_verifier, return_to, expires_at) VALUES ($1, $2, $3, $4, $5, $6)",
		HashToken(state), HashToken(bind), nonce, verifier, SafeReturnPath(returnTo), o.Store.now().Add(oidcLoginTTL))
	if err != nil {
		return "", "", err
	}
	return oc.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), bind, nil
}

// Claims are the ID-token claims the controller uses.
type Claims struct {
	Subject           string
	PreferredUsername string
	Name              string
	Email             string
	Groups            []string
}

// Callback completes a login: it consumes the state, exchanges the code,
// verifies the ID token and its nonce, maps groups to a role, and creates or
// updates the user. It returns the user and the path to return to.
func (o *OIDC) Callback(ctx context.Context, state, code, bind string) (*User, string, error) {
	p, oc, err := o.client(ctx)
	if err != nil {
		return nil, "", err
	}
	var nonce, verifier, returnTo string
	if bind == "" {
		return nil, "", ErrOIDCState // the cookie that binds the login to the browser is missing
	}
	err = o.Store.Pool.QueryRow(ctx, "DELETE FROM oidc_login WHERE state_hash = $1 AND browser_hash = $2 AND expires_at > $3 RETURNING nonce, code_verifier, return_to",
		HashToken(state), HashToken(bind), o.Store.now()).Scan(&nonce, &verifier, &returnTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrOIDCState
	}
	if err != nil {
		return nil, "", err
	}
	tok, err := oc.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, "", fmt.Errorf("auth: OIDC code exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, "", errors.New("auth: OIDC response has no id_token")
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: o.Config.ClientID, Now: o.Store.now}).Verify(ctx, raw)
	if err != nil {
		return nil, "", fmt.Errorf("auth: OIDC ID token: %w", err)
	}
	if idt.Nonce != nonce {
		return nil, "", errors.New("auth: OIDC nonce mismatch")
	}
	var all map[string]any
	if err := idt.Claims(&all); err != nil {
		return nil, "", err
	}
	c := Claims{Subject: idt.Subject}
	c.PreferredUsername, _ = all["preferred_username"].(string)
	c.Name, _ = all["name"].(string)
	c.Email, _ = all["email"].(string)
	c.Groups = groupsClaim(all[o.Config.GroupsClaim])
	u, err := o.upsert(ctx, idt.Issuer, c)
	if err != nil {
		return nil, "", err
	}
	return u, returnTo, nil
}

// groupsClaim accepts a list of strings or a single string.
func groupsClaim(v any) []string {
	switch g := v.(type) {
	case string:
		return []string{g}
	case []any:
		var out []string
		for _, x := range g {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// MapRole applies the group→role mapping: admin if any admin group matches;
// otherwise user if the user groups are empty or one matches; otherwise no
// access.
func MapRole(cfg config.OIDC, groups []string) (Role, bool) {
	has := func(want []string) bool {
		for _, g := range groups {
			if slices.Contains(want, g) {
				return true
			}
		}
		return false
	}
	switch {
	case has(cfg.AdminGroups):
		return RoleAdmin, true
	case len(cfg.UserGroups) == 0 || has(cfg.UserGroups):
		return RoleUser, true
	}
	return "", false
}

func (o *OIDC) upsert(ctx context.Context, issuer string, c Claims) (*User, error) {
	role, ok := MapRole(o.Config, c.Groups)
	if !ok {
		_ = audit.Record(ctx, o.Store.Pool, audit.Entry{Action: "user.login_denied", ObjectType: "user",
			Details: map[string]any{"reason": "no matching group", "subject_hash": shortHash(issuer + "\x00" + c.Subject)}})
		return nil, ErrNotAuthorized
	}
	var u *User
	err := pgx.BeginFunc(ctx, o.Store.Pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		var oldRole string
		var disabled bool
		err := tx.QueryRow(ctx, "SELECT id, role, disabled FROM app_user WHERE oidc_issuer = $1 AND oidc_subject = $2 FOR UPDATE", issuer, c.Subject).
			Scan(&id, &oldRole, &disabled)
		var email *string
		if c.Email != "" {
			email = &c.Email
		}
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			name := o.username(ctx, tx, issuer, c)
			err = tx.QueryRow(ctx, `INSERT INTO app_user (username, display_name, email, role, auth_source, oidc_issuer, oidc_subject)
				VALUES ($1, $2, $3, $4, 'oidc', $5, $6) RETURNING id`, name, c.Name, email, string(role), issuer, c.Subject).Scan(&id)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{Action: "user.create", ObjectType: "user", ObjectID: id.String(),
				Details: map[string]any{"username": name, "role": string(role), "auth_source": "oidc"}})
		case err != nil:
			return err
		case disabled:
			return ErrNotAuthorized
		}
		if _, err := tx.Exec(ctx, "UPDATE app_user SET display_name = $2, email = $3, role = $4 WHERE id = $1", id, c.Name, email, string(role)); err != nil {
			return err
		}
		if oldRole != string(role) {
			return audit.Record(ctx, tx, audit.Entry{Action: "user.role_change", ObjectType: "user", ObjectID: id.String(),
				Details: map[string]any{"from": oldRole, "to": string(role), "source": "oidc groups"}})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var id uuid.UUID
	if err := o.Store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE oidc_issuer = $1 AND oidc_subject = $2", issuer, c.Subject).Scan(&id); err != nil {
		return nil, err
	}
	u, err = o.Store.loggedIn(ctx, id, "oidc")
	return u, err
}

// username picks a username for a new OIDC user: preferred_username, then
// e-mail, then the subject. If that is taken (for example by a local
// account), a short hash of the identity is appended, so an IdP user can
// never take over an existing account by claiming its name.
func (o *OIDC) username(ctx context.Context, tx pgx.Tx, issuer string, c Claims) string {
	name := c.PreferredUsername
	if ValidUsername(name) != nil {
		name = c.Email
	}
	if ValidUsername(name) != nil {
		name = "oidc-" + shortHash(issuer+"\x00"+c.Subject)
	}
	if len(name) > 100 {
		name = strings.ToValidUTF8(name[:100], "")
	}
	var taken bool
	err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM app_user WHERE lower(username) = lower($1))", name).Scan(&taken)
	if taken || err != nil { // on error, take the always-unique name
		name += "-" + shortHash(issuer+"\x00"+c.Subject)
	}
	return name
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}
