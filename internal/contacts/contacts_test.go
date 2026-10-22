// SPDX-License-Identifier: AGPL-3.0-or-later

package contacts_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
)

var ctx = context.Background()

func setup(t *testing.T) (*contacts.Store, *auth.User, *auth.User, *auth.User) {
	pool := dbtest.Migrated(t)
	us := &auth.Store{Pool: pool, Argon: auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1}, SessionIdle: time.Hour, SessionMaxAge: time.Hour, RateLimit: 5}
	mk := func(n string, r auth.Role) *auth.User {
		u, err := us.CreateLocalUser(ctx, auth.NewLocalUser{Username: n, Password: "long password", Role: r})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	return &contacts.Store{Pool: pool}, mk("admin", auth.RoleAdmin), mk("anna", auth.RoleUser), mk("ben", auth.RoleUser)
}

func handset(t *testing.T, pool *pgxpool.Pool, owner *uuid.UUID, tpui int) uuid.UUID {
	var id uuid.UUID
	if err := pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, $2, $3, '\\x') RETURNING id",
		owner, []byte{byte(tpui)}, tpui).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestContactsAccess(t *testing.T) {
	s, admin, anna, ben := setup(t)
	mine, err := s.Create(ctx, anna, contacts.Contact{LastName: "Meyer", FirstName: "Eva",
		Numbers: []contacts.Number{{Number: "030 123-45 67", Kind: "fixed"}, {Number: "+49 171 1234567", Kind: "mobile"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if mine.Numbers[0].Number != "0301234567" || !mine.Numbers[0].Default || mine.Numbers[1].Default {
		t.Errorf("numbers %+v", mine.Numbers)
	}
	shared, err := s.Create(ctx, admin, contacts.Contact{LastName: "Reception", Numbers: []contacts.Number{{Number: "100"}}}, true)
	if err != nil || shared.OwnerID != nil {
		t.Fatalf("shared: %+v %v", shared, err)
	}
	if _, err := s.Create(ctx, anna, contacts.Contact{LastName: "x"}, true); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("user created a shared contact: %v", err)
	}
	// Anna sees hers and the shared one; Ben only the shared one.
	if l, _ := s.List(ctx, anna, "", ""); len(l) != 2 {
		t.Errorf("anna sees %d", len(l))
	}
	if l, _ := s.List(ctx, ben, "", ""); len(l) != 1 || l[0].ID != shared.ID {
		t.Errorf("ben sees %v", l)
	}
	if _, err := s.Get(ctx, ben, mine.ID); !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("ben read anna's: %v", err)
	}
	// Shared contacts are readable by users, writable by admins only.
	if _, err := s.Update(ctx, ben, shared.ID, contacts.Contact{LastName: "Hacked"}); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("user edited a shared contact: %v", err)
	}
	if err := s.Delete(ctx, ben, mine.ID); !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("ben deleted anna's: %v", err)
	}
	// Validation.
	_, err = s.Create(ctx, anna, contacts.Contact{Numbers: []contacts.Number{{Number: "abc"}, {Number: "1", Kind: "pager"}}}, false)
	var v contacts.ValidationError
	if !errors.As(err, &v) || v["name"] == "" || v["numbers[0].number"] == "" || v["numbers[1].kind"] == "" {
		t.Errorf("validation: %v", err)
	}
	up, err := s.Update(ctx, anna, mine.ID, contacts.Contact{LastName: "Meyer", FirstName: "Eva", Numbers: []contacts.Number{{Number: "0301234567", Default: true}}})
	if err != nil || len(up.Numbers) != 1 {
		t.Fatalf("update: %+v %v", up, err)
	}
}

func TestHandsetViews(t *testing.T) {
	s, admin, anna, _ := setup(t)
	pool := s.Pool
	annas := handset(t, pool, &anna.ID, 1)
	sharedHS := handset(t, pool, nil, 2)
	mine, _ := s.Create(ctx, anna, contacts.Contact{FirstName: "Eva", LastName: "Meyer", Numbers: []contacts.Number{{Number: "0301234567"}}}, false)
	s.Create(ctx, admin, contacts.Contact{LastName: "Reception", Numbers: []contacts.Number{{Number: "100"}}}, true)

	if l, _ := s.ForHandset(ctx, annas); len(l) != 2 {
		t.Errorf("anna's handset reads %d", len(l))
	}
	if l, _ := s.ForHandset(ctx, sharedHS); len(l) != 1 || l[0].LastName != "Reception" {
		t.Errorf("shared handset reads %v", l)
	}
	// Edits from a handset go to the owner's list; shared handsets are read-only.
	c, err := s.SaveFromHandset(ctx, annas, 0, contacts.Contact{LastName: "New", Numbers: []contacts.Number{{Number: "5"}}})
	if err != nil || c.OwnerID == nil || *c.OwnerID != anna.ID {
		t.Fatalf("handset create: %+v %v", c, err)
	}
	if _, err := s.SaveFromHandset(ctx, sharedHS, 0, contacts.Contact{LastName: "x"}); !errors.Is(err, contacts.ErrReadOnly) {
		t.Errorf("shared handset wrote: %v", err)
	}
	reception, _ := s.List(ctx, anna, "shared", "")
	if _, err := s.SaveFromHandset(ctx, annas, reception[0].ID, contacts.Contact{LastName: "x"}); !errors.Is(err, contacts.ErrReadOnly) {
		t.Errorf("handset edited a shared contact: %v", err)
	}

	// Call lists: incoming by line (account), outgoing by handset.
	var acct uuid.UUID
	pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'l', 'u', 'd') RETURNING id", anna.ID).Scan(&acct)
	pool.Exec(ctx, "INSERT INTO handset_line (handset_id, line, sip_account_id) VALUES ($1, 1, $2)", annas, acct)
	other := handset(t, pool, &anna.ID, 3)
	now := time.Now()
	ins := func(h *uuid.UUID, dir, party string, answered bool, ago time.Duration) int64 {
		var id int64
		var ans *time.Time
		if answered {
			a := now.Add(-ago)
			ans = &a
		}
		pool.QueryRow(ctx, `INSERT INTO call_log (handset_id, owner_user_id, line, sip_account_id, direction, remote_party, started_at, answered_at)
			VALUES ($1, $2, 1, $3, $4, $5, $6, $7) RETURNING id`, h, anna.ID, acct, dir, party, now.Add(-ago), ans).Scan(&id)
		return id
	}
	missed := ins(nil, "in", "0301234567", false, time.Minute) // rang everywhere, nobody answered
	ins(&other, "in", "999", true, 2*time.Minute)              // answered elsewhere: still on this line's list
	ins(&annas, "out", "0301234567", true, 3*time.Minute)
	ins(&other, "out", "777", true, 4*time.Minute) // another handset's dialling

	l, err := s.CallsForHandset(ctx, annas, contacts.Missed, 10)
	if err != nil || len(l) != 1 || l[0].ID != missed || l[0].Name != "Eva Meyer" {
		t.Fatalf("missed: %+v %v", l, err)
	}
	if l, _ := s.CallsForHandset(ctx, annas, contacts.AllIncoming, 10); len(l) != 2 {
		t.Errorf("all incoming: %d", len(l))
	}
	if l, _ := s.CallsForHandset(ctx, annas, contacts.Outgoing, 10); len(l) != 1 || l[0].Number != "0301234567" {
		t.Errorf("outgoing: %+v", l)
	}
	if _, err := s.CallsForHandset(ctx, annas, "voicemail", 10); err == nil {
		t.Error("unknown list accepted")
	}
	// One read mark, seen by every handset on the line.
	if err := s.MarkMissedRead(ctx, anna, missed); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.CallsForHandset(ctx, annas, contacts.Missed, 10); l[0].MissedRead == nil {
		t.Error("read mark not set")
	}
	_ = mine
}
