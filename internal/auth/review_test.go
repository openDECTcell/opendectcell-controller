// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

// Findings W6, W7, W14 and R3 of the review of 2026-11-03.

// Of two parallel logins with one TOTP code, one wins.
func TestTOTPCodeIsUsedOnce(t *testing.T) {
	s, _ := newStore(t)
	ks, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(ks)
	var err error
	if s.Keyring, err = db.LoadKeyring(ctx, s.Pool, mk, nil); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateLocalUser(ctx, auth.NewLocalUser{Username: "tina", Password: "long enough pw", Role: auth.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := s.BeginTOTP(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := auth.TOTPCode(sec, time.Now().Add(-auth.TOTPPeriod*time.Second)) // the step before now: confirmation takes it...
	if _, err := s.ConfirmTOTP(ctx, u.ID, code); err != nil {
		t.Fatal(err)
	}
	next, _ := auth.TOTPCode(sec, time.Now()) // ...so this one is unused
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.VerifySecondFactor(ctx, u.ID, next, ""); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("one code won %d logins", won)
	}
}

// An administrator disables a sign-up that has not followed its link; the link must not open it. Nor may a
// second account take an address another one verified.
func TestVerificationRespectsAdminAndOtherAccounts(t *testing.T) {
	s, _ := newStore(t)
	admin, _ := s.CreateLocalUser(ctx, auth.NewLocalUser{Username: "boss", Password: "long enough pw", Role: auth.RoleAdmin})
	reg := func(name, email string) *auth.User {
		u, err := s.RegisterSelf(ctx, auth.SelfRegistration{Username: name, Email: email, Password: "long enough pw", Unverified: true}, "")
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	verify := func(u *auth.User) (*auth.User, error) {
		tok, err := s.NewEmailToken(ctx, u.ID, auth.TokenVerify, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return s.VerifyEmail(ctx, tok)
	}

	a := reg("anna", "anna@example.org")
	off := true
	if _, err := s.UpdateUser(ctx, admin, a.ID, auth.UserUpdate{Disabled: &off}); err != nil {
		t.Fatal(err)
	}
	if got, err := verify(a); err != nil || !got.Disabled {
		t.Errorf("an account an admin disabled is open after its link: %+v %v", got, err)
	}

	b := reg("ben", "Ben@Example.org")
	c := reg("carl", "ben@example.org") // the same address, not yet verified
	if _, err := verify(b); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(c); !errors.Is(err, auth.ErrEmailInUse) {
		t.Errorf("a second account verified the same address: %v", err)
	}
	// resend: an address two sign-ups wait under is ambiguous and gets no link
	d, e := reg("dora", "dora@example.org"), reg("emil", "dora@example.org")
	_, _ = d, e
	if got, err := s.RequestVerification(ctx, "dora@example.org", ""); err != nil || got != nil {
		t.Errorf("an ambiguous address got a resend: %v %v", got, err)
	}
	f := reg("fred", "fred@example.org")
	if got, err := s.RequestVerification(ctx, "fred@example.org", ""); err != nil || got == nil || got.ID != f.ID {
		t.Errorf("a clear address got no resend: %v %v", got, err)
	}
}

// What was typed into the name field of a failed login, maybe a password, is not kept.
func TestFailedLoginAuditKeepsOnlyKnownNames(t *testing.T) {
	s, _ := newStore(t)
	s.CreateLocalUser(ctx, auth.NewLocalUser{Username: "gina", Password: "long enough pw", Role: auth.RoleUser})
	s.CheckPassword1(ctx, "gina", "wrong password", "10.0.0.1")
	s.CheckPassword1(ctx, "hunter2-my-password", "x", "10.0.0.2")
	var names []string
	rows, err := s.Pool.Query(ctx, "SELECT details->>'username' FROM audit_log WHERE action = 'user.login_failed' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	if len(names) != 2 || names[0] != "gina" || names[1] != "(no such user)" {
		t.Errorf("audited names: %v", names)
	}
}
