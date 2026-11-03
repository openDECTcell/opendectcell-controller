// SPDX-License-Identifier: AGPL-3.0-or-later

// Package org holds organizations and the trust setting (docs/adr/0007-organizations-and-trust.md).
// An organization groups users so that others can decide to trust them: it gives
// its members no rights over each other's handsets, base stations or accounts.
// Whether a handset may be put on a base station is decided by one SQL function,
// handset_may_use_cell, which reads what this package writes.
package org

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

// Role is a member's role in one organization. The owner is an admin who is also
// named in organization.owner_user_id.
type Role string

const (
	// RoleAdmin is a member who may invite and remove members.
	RoleAdmin Role = "admin"
	// RoleMember is a member who may only see the organization, name it for themselves, trust it and leave.
	RoleMember Role = "member"
)

// Scope is whose base stations a user's handsets may use.
type Scope string

const (
	// ScopeAll is trust in every base station of the controller.
	ScopeAll Scope = "all"
	// ScopeOwn is trust in the user's own base stations only.
	ScopeOwn Scope = "own"
	// ScopeOrgs is trust in the user's own base stations and those of the members of the organizations they trust.
	ScopeOrgs Scope = "orgs"
)

// Limits that keep one user from filling the tables.
const (
	MaxOrgsPerUser    = 20
	MaxMembers        = 500
	MaxOpenInvites    = 50
	InviteTTL         = 7 * 24 * time.Hour
	maxNameLen        = 64
	minTokenLen       = 20
	maxTokenLen       = 128
	maxNameLenMessage = "A name is at most 64 characters."
)

// What the store answers when an action cannot be done.
var (
	ErrNotFound      = errors.New("org: not found")
	ErrForbidden     = errors.New("org: not allowed")
	ErrBadToken      = errors.New("org: this invitation is not valid (it may have expired or been used)")
	ErrAlreadyMember = errors.New("org: you are already a member")
	ErrTooMany       = errors.New("org: too many")
	ErrOwner         = errors.New("org: the owner cannot be removed, demoted or leave; delete the organization instead")
	ErrBadScope      = errors.New("org: unknown trust setting")
)

// ValidationError says what is wrong with a name the user typed.
type ValidationError string

func (e ValidationError) Error() string { return string(e) }

// Enforcer is told when what a user's handsets may use has changed, so records
// that are no longer allowed leave the base stations' tables.
type Enforcer interface{ EnforceTrust() }

// Store reads and writes organizations.
type Store struct {
	Pool *pgxpool.Pool
	// Enforcer, if set, is called after every change that can take trust away.
	Enforcer Enforcer
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) changed() {
	if s.Enforcer != nil {
		s.Enforcer.EnforceTrust()
	}
}

// Org is an organization as its owner named it.
type Org struct {
	ID        uuid.UUID
	Name      string
	OwnerID   *uuid.UUID
	CreatedAt time.Time
}

// Mine is an organization as one member sees it.
type Mine struct {
	Org
	Display string // what the member calls it
	Role    Role
	Owner   bool
	Members int
	Trusted bool
}

// Member is one member as the others see them: a name and a number of cells.
type Member struct {
	UserID uuid.UUID
	Name   string
	Role   Role
	Owner  bool
	Cells  int
	Joined time.Time
}

// Counts is a total and how many of it are online.
type Counts struct{ Total, Online int }

// Offline is the rest.
func (c Counts) Offline() int { return c.Total - c.Online }

// Overview is what a member sees of an organization.
type Overview struct {
	Mine
	Cells    Counts
	Handsets Counts
	People   []Member
	// Can says what the viewer may do here.
	CanInvite, CanRemove, CanAppoint, CanDelete bool
	OpenInvites                                 int
}

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ValidationError("A name is required.")
	}
	if utf8.RuneCountInString(s) > maxNameLen {
		return "", ValidationError(maxNameLenMessage)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", ValidationError("A name has no control characters.")
		}
	}
	return s, nil
}

// standing is a viewer's standing in an organization.
type standing struct {
	member, admin, owner, system bool
}

func (st standing) any() bool { return st.member || st.system }

// manage: owner or controller admin. run: admin of the organization, its owner or a controller admin.
func (st standing) manage() bool { return st.owner || st.system }
func (st standing) run() bool    { return st.admin || st.owner || st.system }

func (s *Store) standing(ctx context.Context, q pgx.Tx, u *auth.User, org uuid.UUID) (standing, error) {
	st := standing{system: u.IsAdmin()}
	var role *string // NULL: not a member
	var owner *uuid.UUID
	err := q.QueryRow(ctx, `SELECT m.role, o.owner_user_id FROM organization o
		LEFT JOIN organization_member m ON m.org_id = o.id AND m.user_id = $2 WHERE o.id = $1`, org, u.ID).Scan(&role, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrNotFound
	}
	if err != nil {
		return st, err
	}
	st.member = role != nil
	st.admin = role != nil && *role == string(RoleAdmin)
	st.owner = owner != nil && *owner == u.ID
	if !st.any() {
		return st, ErrNotFound // what a non-member must not learn: that it exists
	}
	return st, nil
}

// Create makes an organization the user owns and administers.
func (s *Store) Create(ctx context.Context, u *auth.User, name string) (*Org, error) {
	name, err := cleanName(name)
	if err != nil {
		return nil, err
	}
	var o Org
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM organization WHERE owner_user_id = $1", u.ID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxOrgsPerUser {
			return ErrTooMany
		}
		if err := tx.QueryRow(ctx, `INSERT INTO organization (name, owner_user_id) VALUES ($1, $2) RETURNING id, name, owner_user_id, created_at`,
			name, u.ID).Scan(&o.ID, &o.Name, &o.OwnerID, &o.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO organization_member (org_id, user_id, role) VALUES ($1, $2, 'admin')", o.ID, u.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "org.create", ObjectType: "organization", ObjectID: o.ID.String(), Details: map[string]any{"name": name}})
	})
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ListMine returns the organizations u belongs to.
func (s *Store) ListMine(ctx context.Context, u *auth.User) ([]Mine, error) {
	rows, err := s.Pool.Query(ctx, `SELECT o.id, o.name, o.owner_user_id, o.created_at, coalesce(m.display_name, o.name), m.role,
			(SELECT count(*) FROM organization_member x WHERE x.org_id = o.id),
			EXISTS (SELECT 1 FROM user_trusted_org t WHERE t.org_id = o.id AND t.user_id = $1)
		FROM organization o JOIN organization_member m ON m.org_id = o.id AND m.user_id = $1
		ORDER BY lower(coalesce(m.display_name, o.name)), o.id`, u.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Mine, error) {
		var m Mine
		var role string
		err := r.Scan(&m.ID, &m.Name, &m.OwnerID, &m.CreatedAt, &m.Display, &role, &m.Members, &m.Trusted)
		m.Role = Role(role)
		m.Owner = m.OwnerID != nil && *m.OwnerID == u.ID
		return m, err
	})
}

// ListAll returns every organization, for a controller admin.
func (s *Store) ListAll(ctx context.Context, u *auth.User) ([]Mine, error) {
	if !u.IsAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.Pool.Query(ctx, `SELECT o.id, o.name, o.owner_user_id, o.created_at, o.name,
			(SELECT count(*) FROM organization_member x WHERE x.org_id = o.id)
		FROM organization o ORDER BY lower(o.name), o.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Mine, error) {
		var m Mine
		err := r.Scan(&m.ID, &m.Name, &m.OwnerID, &m.CreatedAt, &m.Display, &m.Members)
		return m, err
	})
}

// Overview is the organization page for u. cellOnline says whether a cell is
// connected right now; handsets counts the handsets of the given owners and how
// many of them are online.
func (s *Store) Overview(ctx context.Context, u *auth.User, id uuid.UUID, cellOnline func(uuid.UUID) bool,
	handsets func(ctx context.Context, owners []uuid.UUID) (Counts, error)) (*Overview, error) {
	var ov Overview
	var st standing
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		if st, err = s.standing(ctx, tx, u, id); err != nil {
			return err
		}
		var role *string
		var alias *string
		err = tx.QueryRow(ctx, `SELECT o.id, o.name, o.owner_user_id, o.created_at, m.display_name, m.role,
				(SELECT count(*) FROM organization_member x WHERE x.org_id = o.id),
				EXISTS (SELECT 1 FROM user_trusted_org t WHERE t.org_id = o.id AND t.user_id = $2),
				(SELECT count(*) FROM organization_invite i WHERE i.org_id = o.id AND i.used_at IS NULL AND i.expires_at > $3)
			FROM organization o LEFT JOIN organization_member m ON m.org_id = o.id AND m.user_id = $2 WHERE o.id = $1`,
			id, u.ID, s.now()).Scan(&ov.ID, &ov.Name, &ov.OwnerID, &ov.CreatedAt, &alias, &role, &ov.Members, &ov.Trusted, &ov.OpenInvites)
		if err != nil {
			return err
		}
		ov.Display = ov.Name
		if alias != nil {
			ov.Display = *alias
		}
		if role != nil {
			ov.Role = Role(*role)
		}
		ov.Owner = st.owner
		return nil
	})
	if err != nil {
		return nil, err
	}
	ov.CanInvite, ov.CanRemove = st.run(), st.run()
	ov.CanAppoint, ov.CanDelete = st.manage(), st.manage()
	if err = s.fillMembers(ctx, &ov, cellOnline); err != nil {
		return nil, err
	}
	var owners []uuid.UUID
	for _, m := range ov.People {
		owners = append(owners, m.UserID)
	}
	if handsets != nil {
		if ov.Handsets, err = handsets(ctx, owners); err != nil {
			return nil, err
		}
	}
	return &ov, nil
}

// fillMembers loads the members with their number of cells and sums the cells of the organization.
func (s *Store) fillMembers(ctx context.Context, ov *Overview, cellOnline func(uuid.UUID) bool) error {
	rows, err := s.Pool.Query(ctx, `SELECT m.user_id, coalesce(nullif(u.display_name, ''), u.username), m.role, m.joined_at,
			coalesce((SELECT array_agg(c.id) FROM cell c WHERE c.enrolled_by = m.user_id AND c.state <> 'revoked'), '{}')
		FROM organization_member m JOIN app_user u ON u.id = m.user_id
		WHERE m.org_id = $1 ORDER BY lower(coalesce(nullif(u.display_name, ''), u.username)), m.user_id`, ov.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m Member
		var role string
		var cells []uuid.UUID
		if err := rows.Scan(&m.UserID, &m.Name, &role, &m.Joined, &cells); err != nil {
			return err
		}
		m.Role, m.Cells = Role(role), len(cells)
		m.Owner = ov.OwnerID != nil && *ov.OwnerID == m.UserID
		ov.Cells.Total += len(cells)
		for _, c := range cells {
			if cellOnline != nil && cellOnline(c) {
				ov.Cells.Online++
			}
		}
		ov.People = append(ov.People, m)
	}
	return rows.Err()
}

// SetAlias sets the name u calls an organization by; "" goes back to its own name.
func (s *Store) SetAlias(ctx context.Context, u *auth.User, id uuid.UUID, alias string) error {
	var v *string
	if strings.TrimSpace(alias) != "" {
		c, err := cleanName(alias)
		if err != nil {
			return err
		}
		v = &c
	}
	tag, err := s.Pool.Exec(ctx, "UPDATE organization_member SET display_name = $3 WHERE org_id = $1 AND user_id = $2", id, u.ID, v)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Rename changes the organization's own name (owner or controller admin).
func (s *Store) Rename(ctx context.Context, u *auth.User, id uuid.UUID, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		st, err := s.standing(ctx, tx, u, id)
		if err != nil {
			return err
		}
		if !st.manage() {
			return ErrForbidden
		}
		if _, err := tx.Exec(ctx, "UPDATE organization SET name = $2 WHERE id = $1", id, name); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "org.rename", ObjectType: "organization", ObjectID: id.String(), Details: map[string]any{"name": name}})
	})
}

// Delete removes an organization (owner or controller admin). Trust in it ends with it.
func (s *Store) Delete(ctx context.Context, u *auth.User, id uuid.UUID) error {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		st, err := s.standing(ctx, tx, u, id)
		if err != nil {
			return err
		}
		if !st.manage() {
			return ErrForbidden
		}
		if _, err := tx.Exec(ctx, "DELETE FROM organization WHERE id = $1", id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "org.delete", ObjectType: "organization", ObjectID: id.String()})
	})
	if err == nil {
		s.changed()
	}
	return err
}

// SetRole makes a member an admin or an ordinary member (owner or controller admin).
func (s *Store) SetRole(ctx context.Context, u *auth.User, id, member uuid.UUID, role Role) error {
	if role != RoleAdmin && role != RoleMember {
		return ValidationError("Unknown role.")
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		st, err := s.standing(ctx, tx, u, id)
		if err != nil {
			return err
		}
		if !st.manage() {
			return ErrForbidden
		}
		var owner *uuid.UUID
		if err := tx.QueryRow(ctx, "SELECT owner_user_id FROM organization WHERE id = $1", id).Scan(&owner); err != nil {
			return err
		}
		if owner != nil && *owner == member {
			return ErrOwner
		}
		tag, err := tx.Exec(ctx, "UPDATE organization_member SET role = $3 WHERE org_id = $1 AND user_id = $2", id, member, string(role))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "org.role", ObjectType: "organization", ObjectID: id.String(),
			Details: map[string]any{"user": member.String(), "role": string(role)}})
	})
}

// Remove takes a member out. An admin removes members; only the owner (or a controller admin) removes admins;
// nobody removes the owner.
func (s *Store) Remove(ctx context.Context, u *auth.User, id, member uuid.UUID) error {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		st, err := s.standing(ctx, tx, u, id)
		if err != nil {
			return err
		}
		if !st.run() {
			return ErrForbidden
		}
		var owner *uuid.UUID
		var role string
		if err := tx.QueryRow(ctx, "SELECT owner_user_id FROM organization WHERE id = $1", id).Scan(&owner); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, "SELECT role FROM organization_member WHERE org_id = $1 AND user_id = $2", id, member).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if owner != nil && *owner == member {
			return ErrOwner
		}
		if role == string(RoleAdmin) && !st.manage() {
			return ErrForbidden
		}
		if _, err := tx.Exec(ctx, "DELETE FROM organization_member WHERE org_id = $1 AND user_id = $2", id, member); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "org.remove_member", ObjectType: "organization", ObjectID: id.String(),
			Details: map[string]any{"user": member.String()}})
	})
	if err == nil {
		s.changed()
	}
	return err
}

// Leave removes u from an organization; the owner cannot.
func (s *Store) Leave(ctx context.Context, u *auth.User, id uuid.UUID) error {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var owner *uuid.UUID
		if err := tx.QueryRow(ctx, "SELECT owner_user_id FROM organization WHERE id = $1", id).Scan(&owner); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if owner != nil && *owner == u.ID {
			return ErrOwner
		}
		tag, err := tx.Exec(ctx, "DELETE FROM organization_member WHERE org_id = $1 AND user_id = $2", id, u.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "org.leave", ObjectType: "organization", ObjectID: id.String()})
	})
	if err == nil {
		s.changed()
	}
	return err
}

// CreateInvite makes a single-use token for the organization (admins, the owner, controller admins).
// The token is returned once; only its hash is kept.
func (s *Store) CreateInvite(ctx context.Context, u *auth.User, id uuid.UUID) (token string, expires time.Time, err error) {
	token, err = auth.NewToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires = s.now().Add(InviteTTL)
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		st, err := s.standing(ctx, tx, u, id)
		if err != nil {
			return err
		}
		if !st.run() {
			return ErrForbidden
		}
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM organization_invite WHERE org_id = $1 AND used_at IS NULL AND expires_at > $2", id, s.now()).Scan(&n); err != nil {
			return err
		}
		if n >= MaxOpenInvites {
			return ErrTooMany
		}
		if _, err := tx.Exec(ctx, "INSERT INTO organization_invite (token_hash, org_id, created_by, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)",
			auth.HashToken(token), id, u.ID, s.now(), expires); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "org.invite", ObjectType: "organization", ObjectID: id.String()})
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

func tokenShape(tok string) bool { return len(tok) >= minTokenLen && len(tok) <= maxTokenLen }

// PeekInvite says which organization a token is for, without using it.
func (s *Store) PeekInvite(ctx context.Context, tok string) (*Org, error) {
	tok = strings.TrimSpace(tok)
	if !tokenShape(tok) {
		return nil, ErrBadToken
	}
	var o Org
	err := s.Pool.QueryRow(ctx, `SELECT o.id, o.name, o.owner_user_id, o.created_at FROM organization_invite i JOIN organization o ON o.id = i.org_id
		WHERE i.token_hash = $1 AND i.used_at IS NULL AND i.expires_at > $2`, auth.HashToken(tok), s.now()).Scan(&o.ID, &o.Name, &o.OwnerID, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBadToken
	}
	return &o, err
}

// Join uses a token: u becomes a member, calling the organization alias (or by its own name when alias is empty).
// A user who is already a member keeps the token unused.
func (s *Store) Join(ctx context.Context, u *auth.User, tok, alias string) (*Org, error) {
	tok = strings.TrimSpace(tok)
	if !tokenShape(tok) {
		return nil, ErrBadToken
	}
	var v *string
	if strings.TrimSpace(alias) != "" {
		c, err := cleanName(alias)
		if err != nil {
			return nil, err
		}
		v = &c
	}
	var o Org
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT org_id FROM organization_invite WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2 FOR UPDATE`,
			auth.HashToken(tok), s.now()).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBadToken
		}
		if err != nil {
			return err
		}
		var members int
		var already bool
		if err := tx.QueryRow(ctx, `SELECT count(*), bool_or(user_id = $2) FROM organization_member WHERE org_id = $1`, id, u.ID).Scan(&members, &already); err != nil {
			return err
		}
		if already {
			return ErrAlreadyMember
		}
		if members >= MaxMembers {
			return ErrTooMany
		}
		if _, err := tx.Exec(ctx, "UPDATE organization_invite SET used_at = $2, used_by = $3 WHERE token_hash = $1", auth.HashToken(tok), s.now(), u.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO organization_member (org_id, user_id, role, display_name) VALUES ($1, $2, 'member', $3)", id, u.ID, v); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT id, name, owner_user_id, created_at FROM organization WHERE id = $1", id).Scan(&o.ID, &o.Name, &o.OwnerID, &o.CreatedAt); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "org.join", ObjectType: "organization", ObjectID: id.String()})
	})
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// Trust is a user's trust setting.
type Trust struct {
	Scope Scope
	Orgs  map[uuid.UUID]bool
}

// GetTrust reads u's trust setting.
func (s *Store) GetTrust(ctx context.Context, u *auth.User) (Trust, error) {
	t := Trust{Orgs: map[uuid.UUID]bool{}}
	var sc string
	if err := s.Pool.QueryRow(ctx, "SELECT trust_scope FROM app_user WHERE id = $1", u.ID).Scan(&sc); err != nil {
		return t, err
	}
	t.Scope = Scope(sc)
	rows, err := s.Pool.Query(ctx, "SELECT org_id FROM user_trusted_org WHERE user_id = $1", u.ID)
	if err != nil {
		return t, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	for _, id := range ids {
		t.Orgs[id] = true
	}
	return t, err
}

// SetTrust sets whose base stations u's handsets may use. orgs are the organizations to trust when
// scope is "orgs"; ones u does not belong to are ignored. The base stations' tables are cleaned afterwards.
func (s *Store) SetTrust(ctx context.Context, u *auth.User, scope Scope, orgs []uuid.UUID) error {
	if scope != ScopeAll && scope != ScopeOwn && scope != ScopeOrgs {
		return ErrBadScope
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE app_user SET trust_scope = $2 WHERE id = $1", u.ID, string(scope)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM user_trusted_org WHERE user_id = $1", u.ID); err != nil {
			return err
		}
		if scope == ScopeOrgs && len(orgs) > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO user_trusted_org (user_id, org_id)
				SELECT $1, m.org_id FROM organization_member m WHERE m.user_id = $1 AND m.org_id = ANY($2) ON CONFLICT DO NOTHING`, u.ID, orgs); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "user.trust", ObjectType: "user", ObjectID: u.ID.String(), Details: map[string]any{"scope": string(scope), "organizations": len(orgs)}})
	})
	if err == nil {
		s.changed()
	}
	return err
}
