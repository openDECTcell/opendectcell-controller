// SPDX-License-Identifier: AGPL-3.0-or-later

package sipaccount_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
)

var ctx = context.Background()

func ptr[T any](v T) *T { return &v }

func setup(t *testing.T) (*sipaccount.Store, *auth.User, *auth.User, *auth.User, *int) {
	pool := dbtest.Migrated(t)
	ks, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(ks)
	kr, err := db.LoadKeyring(ctx, pool, mk, nil)
	if err != nil {
		t.Fatal(err)
	}
	users := &auth.Store{Pool: pool, Argon: auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1}, SessionIdle: time.Hour, SessionMaxAge: time.Hour, RateLimit: 5}
	mk2 := func(name string, role auth.Role) *auth.User {
		u, err := users.CreateLocalUser(ctx, auth.NewLocalUser{Username: name, Password: "long password", Role: role})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	changes := new(int)
	s := &sipaccount.Store{Pool: pool, Keyring: kr, OnChange: func() { *changes++ }}
	return s, mk2("admin", auth.RoleAdmin), mk2("anna", auth.RoleUser), mk2("ben", auth.RoleUser), changes
}

func input() sipaccount.Input {
	return sipaccount.Input{Label: ptr("Home"), Username: ptr("49301234"), Domain: ptr("sip.example.net"), Password: ptr("s3cr3t-pass"),
		Transport: ptr("udp"), MediaEncryption: ptr("off")}
}

func TestCreateListGetAuthorisation(t *testing.T) {
	s, admin, anna, ben, changes := setup(t)
	a, err := s.Create(audit.WithActor(ctx, anna.Actor()), anna, input())
	if err != nil {
		t.Fatal(err)
	}
	if *a.OwnerID != anna.ID || !a.HasPassword || a.RegisterExpiry != 600 || strings.Join(a.Codecs, ",") != "G722,PCMA,PCMU" || *changes != 1 {
		t.Fatalf("created %+v (changes %d)", a, *changes)
	}
	// Owners and admins see it; others get not-found.
	if _, err := s.Get(ctx, anna, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, admin, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, ben, a.ID); !errors.Is(err, sipaccount.ErrNotFound) {
		t.Fatalf("other user: %v", err)
	}
	if l, _ := s.List(ctx, ben, ""); len(l) != 0 {
		t.Fatal("other user lists it")
	}
	if l, _ := s.List(ctx, admin, anna.ID.String()); len(l) != 1 {
		t.Fatal("admin owner filter")
	}
	// Users cannot choose an owner; admins can make it shared.
	in := input()
	var none *uuid.UUID
	in.OwnerID = &none
	if _, err := s.Create(ctx, anna, in); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("user set owner: %v", err)
	}
	sh, err := s.Create(ctx, admin, in)
	if err != nil || sh.OwnerID != nil {
		t.Fatalf("shared: %+v %v", sh, err)
	}
	if _, err := s.Get(ctx, anna, sh.ID); !errors.Is(err, sipaccount.ErrNotFound) {
		t.Fatal("user reached a shared account")
	}
	if l, _ := s.List(ctx, admin, "shared"); len(l) != 1 {
		t.Fatal("shared filter")
	}
}

func TestValidation(t *testing.T) {
	s, _, anna, _, _ := setup(t)
	bad := input()
	bad.Codecs = &[]string{"GSM"}
	bad.Transport = ptr("sctp")
	bad.Label = ptr("")
	_, err := s.Create(ctx, anna, bad)
	var v sipaccount.ValidationError
	if !errors.As(err, &v) || v["label"] == "" || v["account"] == "" {
		t.Fatalf("validation: %v", err)
	}
	if !strings.Contains(err.Error(), "GSM") || !strings.Contains(err.Error(), "sctp") {
		t.Errorf("message: %v", err)
	}
}

func TestPasswordSealedAndRuntime(t *testing.T) {
	s, _, anna, ben, _ := setup(t)
	a, _ := s.Create(audit.WithActor(ctx, anna.Actor()), anna, input())
	var enc []byte
	s.Pool.QueryRow(ctx, "SELECT password_enc FROM sip_account WHERE id = $1", a.ID).Scan(&enc)
	if bytes.Contains(enc, []byte("s3cr3t-pass")) || len(enc) == 0 {
		t.Fatal("password not sealed")
	}
	rt, err := s.RuntimeAccounts(ctx)
	if err != nil || len(rt) != 1 || rt[0].Password != "s3cr3t-pass" || rt[0].Transport != "udp" {
		t.Fatalf("runtime: %+v %v", rt, err)
	}

	// Update keeps the password unless given; "" clears it.
	up, err := s.Update(ctx, anna, a.ID, sipaccount.Input{Label: ptr("Home line")})
	if err != nil || !up.HasPassword || up.Label != "Home line" {
		t.Fatalf("update: %+v %v", up, err)
	}
	if _, err := s.Update(ctx, ben, a.ID, sipaccount.Input{Label: ptr("x")}); !errors.Is(err, sipaccount.ErrNotFound) {
		t.Fatalf("other user updated: %v", err)
	}
	up, _ = s.Update(ctx, anna, a.ID, sipaccount.Input{Password: ptr("")})
	if up.HasPassword {
		t.Fatal("password not cleared")
	}

	// A sealed value copied into another row does not open there.
	b, _ := s.Create(ctx, anna, input())
	s.Pool.Exec(ctx, "UPDATE sip_account SET password_enc = (SELECT password_enc FROM sip_account WHERE id = $1) WHERE id = $2", b.ID, a.ID)
	rt, err = s.RuntimeAccounts(ctx)
	if err == nil || len(rt) != 1 || rt[0].ID != b.ID {
		t.Fatalf("swapped ciphertext: %d accounts, %v", len(rt), err)
	}

	rows, _ := audit.List(ctx, s.Pool, audit.Filter{ObjectType: "sip_account"})
	for _, r := range rows {
		for k, v := range r.Details {
			if strings.Contains(strings.ToLower(k), "pass") || strings.Contains(strings.ToLower(toString(v)), "s3cr3t") {
				t.Errorf("secret in audit: %v", r.Details)
			}
		}
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var b strings.Builder
		for _, e := range x {
			b.WriteString(toString(e))
		}
		return b.String()
	}
	return ""
}

func TestDeleteInUse(t *testing.T) {
	s, _, anna, ben, _ := setup(t)
	a, _ := s.Create(ctx, anna, input())
	var hid uuid.UUID
	s.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\\x01', 1, '\\x') RETURNING id", anna.ID).Scan(&hid)
	s.Pool.Exec(ctx, "INSERT INTO handset_line (handset_id, line, sip_account_id) VALUES ($1, 1, $2)", hid, a.ID)
	if err := s.Delete(ctx, anna, a.ID); !errors.Is(err, sipaccount.ErrInUse) {
		t.Fatalf("in use: %v", err)
	}
	if err := s.Delete(ctx, ben, a.ID); !errors.Is(err, sipaccount.ErrNotFound) {
		t.Fatalf("other user: %v", err)
	}
	s.Pool.Exec(ctx, "DELETE FROM handset_line")
	if err := s.Delete(ctx, anna, a.ID); err != nil {
		t.Fatal(err)
	}
}
