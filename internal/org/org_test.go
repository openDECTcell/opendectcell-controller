// SPDX-License-Identifier: AGPL-3.0-or-later

package org_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/org"
)

type env struct {
	pool  *pgxpool.Pool
	users *auth.Store
	org   *org.Store
	enf   *enforcer
	clk   time.Time
}

type enforcer struct{ n int }

func (e *enforcer) EnforceTrust() { e.n++ }

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.Migrated(t)
	e := &env{pool: pool, enf: &enforcer{}, clk: time.Now()}
	e.users = &auth.Store{Pool: pool, Argon: auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1}, SessionIdle: time.Hour, SessionMaxAge: time.Hour, RateLimit: 5}
	e.org = &org.Store{Pool: pool, Enforcer: e.enf, Now: func() time.Time { return e.clk }}
	return e
}

func (e *env) user(t *testing.T, name string, role auth.Role) (*auth.User, context.Context) {
	t.Helper()
	u, err := e.users.CreateLocalUser(context.Background(), auth.NewLocalUser{Username: name, Password: "long enough pw", Role: role})
	if err != nil {
		t.Fatal(err)
	}
	return u, audit.WithActor(context.Background(), u.Actor())
}

func (e *env) cell(t *testing.T, name string, operator *auth.User) uuid.UUID {
	t.Helper()
	var by any
	if operator != nil {
		by = operator.ID
	}
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), "INSERT INTO cell (name, state, enrolled_by) VALUES ($1, 'active', $2) RETURNING id", name, by).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) may(t *testing.T, owner *auth.User, cell uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := e.pool.QueryRow(context.Background(), "SELECT handset_may_use_cell($1, $2)", owner.ID, cell).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// join makes member a member of o through an invitation, as the owner would hand it out.
func (e *env) join(t *testing.T, owner *auth.User, ownerCtx context.Context, o *org.Org, member *auth.User, memberCtx context.Context, alias string) {
	t.Helper()
	tok, _, err := e.org.CreateInvite(ownerCtx, owner, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.org.Join(memberCtx, member, tok, alias); err != nil {
		t.Fatal(err)
	}
}

func TestTrustScopes(t *testing.T) {
	e := newEnv(t)
	alice, actx := e.user(t, "alice", auth.RoleUser) // owns the handset
	bob, bctx := e.user(t, "bob", auth.RoleUser)     // runs a cell, member of the organization alice trusts
	carol, _ := e.user(t, "carol", auth.RoleUser)    // runs a cell, in nobody's organization
	ca, cb, cc, cn := e.cell(t, "alice's", alice), e.cell(t, "bob's", bob), e.cell(t, "carol's", carol), e.cell(t, "orphan", nil)

	// The default is the shared network as before: every cell.
	for _, c := range []uuid.UUID{ca, cb, cc, cn} {
		if !e.may(t, alice, c) {
			t.Fatal("default scope refused a cell")
		}
	}

	if err := e.org.SetTrust(actx, alice, org.ScopeOwn, nil); err != nil {
		t.Fatal(err)
	}
	for c, want := range map[uuid.UUID]bool{ca: true, cb: false, cc: false, cn: false} {
		if got := e.may(t, alice, c); got != want {
			t.Errorf("own: cell %v = %v, want %v", c, got, want)
		}
	}
	// Bob never chose anything: his handsets keep the default.
	if !e.may(t, bob, ca) {
		t.Error("a user on the default was refused a cell")
	}

	// Alice makes an organization, bob joins, alice trusts it.
	o, err := e.org.Create(actx, alice, "Makerspace")
	if err != nil {
		t.Fatal(err)
	}
	e.join(t, alice, actx, o, bob, bctx, "")
	if err := e.org.SetTrust(actx, alice, org.ScopeOrgs, []uuid.UUID{o.ID}); err != nil {
		t.Fatal(err)
	}
	for c, want := range map[uuid.UUID]bool{ca: true, cb: true, cc: false, cn: false} {
		if got := e.may(t, alice, c); got != want {
			t.Errorf("orgs: cell %v = %v, want %v", c, got, want)
		}
	}
	// Trusting an organization is not mutual: bob's handsets are still on 'all' here, but
	// if bob chose 'orgs' without trusting it, alice's cell would stay closed to him.
	if err := e.org.SetTrust(bctx, bob, org.ScopeOrgs, nil); err != nil {
		t.Fatal(err)
	}
	if e.may(t, bob, ca) {
		t.Error("an untrusted organization's cell was allowed")
	}
	if !e.may(t, bob, cb) {
		t.Error("bob's own cell was refused")
	}

	// Leaving the organization ends the trust with it, and the cleaning ran.
	before := e.enf.n
	if err := e.org.Leave(bctx, bob, o.ID); err != nil {
		t.Fatal(err)
	}
	if e.enf.n == before {
		t.Error("leaving did not ask for the tables to be cleaned")
	}
	if e.may(t, alice, cb) {
		t.Error("a cell of someone who left stayed trusted")
	}
	// Alice trusts the organization, whoever is in it: when bob comes back, his cell is trusted again.
	e.join(t, alice, actx, o, bob, bctx, "")
	if !e.may(t, alice, cb) {
		t.Error("a cell of a member who came back was not trusted again")
	}
	// Alice leaving ends her trust in it, and rejoining does not bring it back by itself.
	if err := e.org.Leave(actx, alice, o.ID); !errors.Is(err, org.ErrOwner) {
		t.Fatalf("the owner left: %v", err)
	}
	e.join(t, alice, actx, o, carol, context.Background(), "")
	if err := e.org.Leave(context.Background(), carol, o.ID); err != nil {
		t.Fatal(err)
	}
	dave, dctx := e.user(t, "dave", auth.RoleUser)
	e.join(t, alice, actx, o, dave, dctx, "")
	if err := e.org.SetTrust(dctx, dave, org.ScopeOrgs, []uuid.UUID{o.ID}); err != nil {
		t.Fatal(err)
	}
	if !e.may(t, dave, cb) {
		t.Fatal("dave does not trust the organization he trusted")
	}
	if err := e.org.Leave(dctx, dave, o.ID); err != nil {
		t.Fatal(err)
	}
	if e.may(t, dave, cb) {
		t.Error("dave's trust survived his leaving")
	}
	e.join(t, alice, actx, o, dave, dctx, "")
	if e.may(t, dave, cb) {
		t.Error("dave's trust came back with his membership")
	}
	if tr, _ := e.org.GetTrust(dctx, dave); tr.Scope != org.ScopeOrgs || len(tr.Orgs) != 0 {
		t.Errorf("dave's trust after leaving and rejoining: %+v", tr)
	}

	// Deleting the organization ends it too.
	if err := e.org.SetTrust(actx, alice, org.ScopeOrgs, []uuid.UUID{o.ID}); err != nil {
		t.Fatal(err)
	}
	if !e.may(t, alice, cb) {
		t.Fatal("trust not restored")
	}
	if err := e.org.Delete(actx, alice, o.ID); err != nil {
		t.Fatal(err)
	}
	if e.may(t, alice, cb) {
		t.Error("a deleted organization is still trusted")
	}
	if err := e.org.SetTrust(actx, alice, "everyone", nil); !errors.Is(err, org.ErrBadScope) {
		t.Errorf("unknown scope: %v", err)
	}
}

// Trust only covers organizations the user belongs to.
func TestTrustIgnoresOrganizationsOfOthers(t *testing.T) {
	e := newEnv(t)
	alice, actx := e.user(t, "alice", auth.RoleUser)
	bob, bctx := e.user(t, "bob", auth.RoleUser)
	cb := e.cell(t, "bob's", bob)
	o, err := e.org.Create(bctx, bob, "Bob's club")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.org.SetTrust(actx, alice, org.ScopeOrgs, []uuid.UUID{o.ID}); err != nil {
		t.Fatal(err)
	}
	if e.may(t, alice, cb) {
		t.Fatal("alice trusted an organization she is not in")
	}
}

func TestOrganizationRoles(t *testing.T) {
	e := newEnv(t)
	owner, octx := e.user(t, "owner", auth.RoleUser)
	adm, actx := e.user(t, "adm", auth.RoleUser)
	adm2, a2ctx := e.user(t, "adm2", auth.RoleUser)
	mem, mctx := e.user(t, "mem", auth.RoleUser)
	out, outctx := e.user(t, "outsider", auth.RoleUser)
	sys, _ := e.user(t, "sysadmin", auth.RoleAdmin)

	o, err := e.org.Create(octx, owner, "  Club  ")
	if err != nil || o.Name != "Club" || o.OwnerID == nil || *o.OwnerID != owner.ID {
		t.Fatalf("create: %+v %v", o, err)
	}
	if _, err := e.org.Create(octx, owner, "   "); err == nil {
		t.Error("an empty name was accepted")
	}
	if _, err := e.org.Create(octx, owner, strings.Repeat("x", 65)); err == nil {
		t.Error("a 65-character name was accepted")
	}
	e.join(t, owner, octx, o, adm, actx, "Our club")
	e.join(t, owner, octx, o, adm2, a2ctx, "")
	e.join(t, owner, octx, o, mem, mctx, "")

	// Only the owner appoints admins.
	if err := e.org.SetRole(actx, adm, o.ID, mem.ID, org.RoleAdmin); !errors.Is(err, org.ErrForbidden) {
		t.Fatalf("a member appointed an admin without being the owner: %v", err)
	}
	if err := e.org.SetRole(octx, owner, o.ID, adm.ID, org.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := e.org.SetRole(octx, owner, o.ID, adm2.ID, org.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := e.org.SetRole(octx, owner, o.ID, owner.ID, org.RoleMember); !errors.Is(err, org.ErrOwner) {
		t.Fatalf("the owner was demoted: %v", err)
	}

	// Admins invite; members and outsiders do not.
	if _, _, err := e.org.CreateInvite(actx, adm, o.ID); err != nil {
		t.Fatalf("an admin could not invite: %v", err)
	}
	if _, _, err := e.org.CreateInvite(mctx, mem, o.ID); !errors.Is(err, org.ErrForbidden) {
		t.Fatalf("a member invited: %v", err)
	}
	if _, _, err := e.org.CreateInvite(outctx, out, o.ID); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("an outsider learned the organization exists: %v", err)
	}
	// Admins remove members, not admins, never the owner; the owner removes admins.
	if err := e.org.Remove(actx, adm, o.ID, adm2.ID); !errors.Is(err, org.ErrForbidden) {
		t.Fatalf("an admin removed an admin: %v", err)
	}
	if err := e.org.Remove(actx, adm, o.ID, owner.ID); !errors.Is(err, org.ErrOwner) {
		t.Fatalf("the owner was removed: %v", err)
	}
	if err := e.org.Remove(mctx, mem, o.ID, adm.ID); !errors.Is(err, org.ErrForbidden) {
		t.Fatalf("a member removed somebody: %v", err)
	}
	if err := e.org.Remove(actx, adm, o.ID, mem.ID); err != nil {
		t.Fatalf("an admin could not remove a member: %v", err)
	}
	if err := e.org.Remove(octx, owner, o.ID, adm2.ID); err != nil {
		t.Fatalf("the owner could not remove an admin: %v", err)
	}
	if err := e.org.Leave(octx, owner, o.ID); !errors.Is(err, org.ErrOwner) {
		t.Fatalf("the owner left: %v", err)
	}
	// Only the owner or a controller admin renames or deletes.
	if err := e.org.Rename(actx, adm, o.ID, "Other"); !errors.Is(err, org.ErrForbidden) {
		t.Fatalf("an admin renamed the organization: %v", err)
	}
	if err := e.org.Delete(actx, adm, o.ID); !errors.Is(err, org.ErrForbidden) {
		t.Fatalf("an admin deleted the organization: %v", err)
	}
	if err := e.org.Rename(octx, owner, o.ID, "Makers"); err != nil {
		t.Fatal(err)
	}
	// A controller admin can act on an organization they are not in.
	if _, _, err := e.org.CreateInvite(context.Background(), sys, o.ID); err != nil {
		t.Fatalf("a controller admin could not invite: %v", err)
	}
	if err := e.org.Delete(context.Background(), sys, o.ID); err != nil {
		t.Fatalf("a controller admin could not delete: %v", err)
	}
	if mine, _ := e.org.ListMine(octx, owner); len(mine) != 0 {
		t.Errorf("a deleted organization is still listed: %+v", mine)
	}
}

func TestInvitationTokens(t *testing.T) {
	e := newEnv(t)
	owner, octx := e.user(t, "owner", auth.RoleUser)
	guest, gctx := e.user(t, "guest", auth.RoleUser)
	late, lctx := e.user(t, "late", auth.RoleUser)
	o, _ := e.org.Create(octx, owner, "Club")

	tok, exp, err := e.org.CreateInvite(octx, owner, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := e.clk.Add(org.InviteTTL); exp.Sub(want) > time.Second || want.Sub(exp) > time.Second {
		t.Errorf("expiry %v, want about %v", exp, want)
	}
	// Only the hash is stored.
	var n int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM organization_invite WHERE position($1 in encode(token_hash, 'hex')) > 0 OR token_hash = convert_to($1, 'UTF8')", tok).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the token itself is in the table: %d %v", n, err)
	}
	// Peeking uses nothing, and shows the organization's name for the suggestion.
	if got, err := e.org.PeekInvite(gctx, tok); err != nil || got.Name != "Club" {
		t.Fatalf("peek: %+v %v", got, err)
	}
	if _, err := e.org.PeekInvite(gctx, "short"); !errors.Is(err, org.ErrBadToken) {
		t.Fatalf("a made-up token: %v", err)
	}
	// The owner cannot use it on themselves, and that keeps it unused.
	if _, err := e.org.Join(octx, owner, tok, ""); !errors.Is(err, org.ErrAlreadyMember) {
		t.Fatalf("a member joined again: %v", err)
	}
	// A name too long is refused before the token is used.
	if _, err := e.org.Join(gctx, guest, tok, strings.Repeat("y", 65)); err == nil {
		t.Fatal("a 65-character display name was accepted")
	}
	if _, err := e.org.Join(gctx, guest, tok, "My crew"); err != nil {
		t.Fatalf("join: %v", err)
	}
	mine, _ := e.org.ListMine(gctx, guest)
	if len(mine) != 1 || mine[0].Display != "My crew" || mine[0].Name != "Club" || mine[0].Role != org.RoleMember {
		t.Fatalf("what the guest sees: %+v", mine)
	}
	// Single use.
	if _, err := e.org.Join(lctx, late, tok, ""); !errors.Is(err, org.ErrBadToken) {
		t.Fatalf("a used token worked again: %v", err)
	}
	// Expiry.
	tok2, _, _ := e.org.CreateInvite(octx, owner, o.ID)
	e.clk = e.clk.Add(org.InviteTTL + time.Minute)
	if _, err := e.org.Join(lctx, late, tok2, ""); !errors.Is(err, org.ErrBadToken) {
		t.Fatalf("an expired token worked: %v", err)
	}
	// Changing the display name later; empty goes back to the organization's own.
	if err := e.org.SetAlias(gctx, guest, o.ID, "Crew"); err != nil {
		t.Fatal(err)
	}
	if err := e.org.SetAlias(gctx, guest, o.ID, ""); err != nil {
		t.Fatal(err)
	}
	if mine, _ := e.org.ListMine(gctx, guest); mine[0].Display != "Club" {
		t.Errorf("display name after clearing: %q", mine[0].Display)
	}
	if err := e.org.SetAlias(lctx, late, o.ID, "x"); !errors.Is(err, org.ErrNotFound) {
		t.Errorf("a non-member set a display name: %v", err)
	}
}

func TestOverviewCountsCellsAndHandsets(t *testing.T) {
	e := newEnv(t)
	owner, octx := e.user(t, "owner", auth.RoleUser)
	bob, bctx := e.user(t, "bob", auth.RoleUser)
	outsider, outctx := e.user(t, "outsider", auth.RoleUser)
	o, _ := e.org.Create(octx, owner, "Club")
	e.join(t, owner, octx, o, bob, bctx, "")
	c1, c2, c3 := e.cell(t, "c1", owner), e.cell(t, "c2", bob), e.cell(t, "c3", bob)
	e.cell(t, "elsewhere", outsider)
	e.cell(t, "revoked", bob)
	if _, err := e.pool.Exec(context.Background(), "UPDATE cell SET state = 'revoked' WHERE name = 'revoked'"); err != nil {
		t.Fatal(err)
	}
	online := map[uuid.UUID]bool{c1: true, c3: true, c2: false}
	var asked []uuid.UUID
	hs := func(_ context.Context, owners []uuid.UUID) (org.Counts, error) {
		asked = owners
		return org.Counts{Total: 5, Online: 2}, nil
	}
	ov, err := e.org.Overview(bctx, bob, o.ID, func(id uuid.UUID) bool { return online[id] }, hs)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Cells.Total != 3 || ov.Cells.Online != 2 || ov.Cells.Offline() != 1 {
		t.Errorf("cells %+v", ov.Cells)
	}
	if ov.Handsets.Total != 5 || ov.Handsets.Offline() != 3 || len(asked) != 2 {
		t.Errorf("handsets %+v asked %v", ov.Handsets, asked)
	}
	if len(ov.People) != 2 {
		t.Fatalf("members %+v", ov.People)
	}
	for _, m := range ov.People {
		switch m.Name {
		case "owner":
			if m.Cells != 1 || !m.Owner || m.Role != org.RoleAdmin {
				t.Errorf("owner %+v", m)
			}
		case "bob":
			if m.Cells != 2 { // the revoked one does not count
				t.Errorf("bob %+v", m)
			}
		default:
			t.Errorf("unexpected member %+v", m)
		}
	}
	if ov.CanInvite || ov.CanDelete || ov.CanAppoint || ov.CanRemove {
		t.Errorf("a plain member can manage: %+v", ov)
	}
	if ov2, err := e.org.Overview(octx, owner, o.ID, nil, nil); err != nil || !ov2.CanInvite || !ov2.CanDelete || !ov2.CanAppoint {
		t.Errorf("the owner's rights: %+v %v", ov2, err)
	}
	if _, err := e.org.Overview(outctx, outsider, o.ID, nil, nil); !errors.Is(err, org.ErrNotFound) {
		t.Errorf("an outsider saw the overview: %v", err)
	}
}

// A user's deletion leaves the organization without an owner; a controller admin still manages it.
func TestOwnerlessOrganization(t *testing.T) {
	e := newEnv(t)
	owner, octx := e.user(t, "owner", auth.RoleUser)
	bob, bctx := e.user(t, "bob", auth.RoleUser)
	sys, sctx := e.user(t, "sys", auth.RoleAdmin)
	o, _ := e.org.Create(octx, owner, "Club")
	e.join(t, owner, octx, o, bob, bctx, "")
	if _, err := e.pool.Exec(context.Background(), "DELETE FROM app_user WHERE id = $1", owner.ID); err != nil {
		t.Fatal(err)
	}
	ov, err := e.org.Overview(sctx, sys, o.ID, nil, nil)
	if err != nil || ov.OwnerID != nil || len(ov.People) != 1 {
		t.Fatalf("ownerless overview: %+v %v", ov, err)
	}
	if err := e.org.Delete(sctx, sys, o.ID); err != nil {
		t.Fatal(err)
	}
}
