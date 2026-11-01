// SPDX-License-Identifier: AGPL-3.0-or-later

package sipaccount_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
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
	// Only the owner sees it: another user and an admin get not-found, an admin
	// included (the account holds the owner's credentials).
	if _, err := s.Get(ctx, anna, a.ID); err != nil {
		t.Fatal(err)
	}
	for who, u := range map[string]*auth.User{"admin": admin, "other user": ben} {
		if _, err := s.Get(ctx, u, a.ID); !errors.Is(err, sipaccount.ErrNotFound) {
			t.Fatalf("%s: %v", who, err)
		}
		if _, err := s.Update(ctx, u, a.ID, sipaccount.Input{Label: ptr("mine now")}); !errors.Is(err, sipaccount.ErrNotFound) {
			t.Fatalf("%s updated it: %v", who, err)
		}
		if err := s.Delete(ctx, u, a.ID); !errors.Is(err, sipaccount.ErrNotFound) {
			t.Fatalf("%s deleted it: %v", who, err)
		}
	}
	if l, _ := s.List(ctx, ben, ""); len(l) != 0 {
		t.Fatal("other user lists it")
	}
	if l, _ := s.List(ctx, admin, ""); len(l) != 0 {
		t.Fatal("the admin lists another user's account")
	}
	if l, _ := s.List(ctx, admin, anna.ID.String()); len(l) != 0 {
		t.Fatal("the admin's owner filter shows another user's account")
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
	if l, _ := s.List(ctx, admin, ""); len(l) != 1 || l[0].ID != sh.ID {
		t.Fatalf("the admin's own list is the shared account only: %d", len(l))
	}
	if _, err := s.Get(ctx, admin, sh.ID); err != nil {
		t.Fatal("the admin manages shared accounts")
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

// A VoIP profile's options, under the names luci-app-opendectcell uses, round-trip
// through the advanced column; the TURN password is sealed in a column of its
// own, never in the JSON.
func TestProfileOptionsRoundTrip(t *testing.T) {
	s, _, anna, _, _ := setup(t)
	f := false
	in := input()
	in.StunPass = ptr("turn-secret-pw")
	in.Advanced = &sipaccount.Advanced{LineName: "Home", Outbound2: "proxy2.example.net:5070", SIPNat: true, MediaNat: "turn",
		StunServer: "turn.example.net:3478", StunUser: "turner", RTPKeep: "zero", MediaAuto: &f, PTime: "30", DTMF: "auto",
		Rel100: "required", MWI: true, Transfer: &f, EncAuto: &f, MediaEnc: "dtls_srtp", Extra: ";answermode=auto",
		SessionTimers: true, UserAgent: "x"}
	in.MediaEncryption = ptr("mandatory")
	a, err := s.Create(audit.WithActor(ctx, anna.Actor()), anna, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, anna, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Advanced, *in.Advanced) || !got.HasStunPass {
		t.Fatalf("stored %+v, has stunpass %v", got.Advanced, got.HasStunPass)
	}
	if got.Advanced.TransferOn() || got.MediaAutoOn() || got.EncAutoOn() || got.EncChoice() != "dtls_srtp" || got.DTMFChoice() != "auto" {
		t.Errorf("derived: transfer %v mediaauto %v encauto %v enc %q dtmf %q", got.Advanced.TransferOn(), got.MediaAutoOn(),
			got.EncAutoOn(), got.EncChoice(), got.DTMFChoice())
	}
	if w := got.Warnings(); !slices.Contains(w, "dtls_srtp_unsupported") {
		t.Errorf("warnings %v", w)
	}

	// The password sits sealed in its own column, bound to it, and not in the JSON.
	var enc, adv []byte
	if err := s.Pool.QueryRow(ctx, "SELECT stunpass_enc, advanced::text::bytea FROM sip_account WHERE id = $1", a.ID).Scan(&enc, &adv); err != nil {
		t.Fatal(err)
	}
	if len(enc) == 0 || bytes.Contains(enc, []byte("turn-secret-pw")) || bytes.Contains(adv, []byte("turn-secret-pw")) {
		t.Fatal("TURN password not sealed")
	}
	pt, err := s.Keyring.Open(secret.Location{Table: "sip_account", Column: "stunpass_enc", RowID: a.ID.String()}, enc)
	if err != nil || string(pt) != "turn-secret-pw" {
		t.Fatalf("open: %q %v", pt, err)
	}
	if _, err := s.Keyring.Open(secret.Location{Table: "sip_account", Column: "password_enc", RowID: a.ID.String()}, enc); err == nil {
		t.Error("the TURN password opens as the SIP password")
	}
	rows, _ := audit.List(ctx, s.Pool, audit.Filter{ObjectType: "sip_account"})
	for _, r := range rows {
		if strings.Contains(strings.ToLower(toString(r.Details["fields"])), "turn-secret") {
			t.Errorf("secret in audit: %v", r.Details)
		}
	}

	// Unchanged without the password; "" clears it; the SIP password is its own.
	up, err := s.Update(ctx, anna, a.ID, sipaccount.Input{Label: ptr("Home 2")})
	if err != nil || !up.HasStunPass || !up.HasPassword {
		t.Fatalf("update: %+v %v", up, err)
	}
	up, err = s.Update(ctx, anna, a.ID, sipaccount.Input{StunPass: ptr("")})
	if err != nil || up.HasStunPass || !up.HasPassword {
		t.Fatalf("clear: %+v %v", up, err)
	}
	if up, err = s.Update(ctx, anna, a.ID, sipaccount.Input{StunPass: ptr("again")}); err != nil || !up.HasStunPass {
		t.Fatalf("set again: %+v %v", up, err)
	}
	if _, err := s.Update(ctx, anna, a.ID, sipaccount.Input{StunPass: ptr(strings.Repeat("x", 300))}); err == nil {
		t.Error("an over-long TURN password was accepted")
	}

	// The runtime side still starts the account, without the TURN password.
	rt, err := s.RuntimeAccounts(ctx)
	if err != nil || len(rt) != 1 || rt[0].Password != "s3cr3t-pass" || rt[0].UserAgent != "x" {
		t.Fatalf("runtime: %+v %v", rt, err)
	}
}

func TestProfileOptionsValidation(t *testing.T) {
	s, _, anna, _, _ := setup(t)
	for name, adv := range map[string]sipaccount.Advanced{
		"advanced.line_name":   {LineName: "Sechzehn Zeichen!"},
		"advanced.outbound2":   {Outbound2: "proxy.example.net:99999"},
		"advanced.stunserver":  {StunServer: "bad host"},
		"advanced.stun_server": {LegacySTUNServer: "a@b"},
		"advanced.medianat":    {MediaNat: "upnp"},
		"advanced.rtpkeep":     {RTPKeep: "ping"},
		"advanced.ptime":       {PTime: "25"},
		"advanced.dtmf":        {DTMF: "magic"},
		"advanced.rel100":      {Rel100: "maybe"},
		"advanced.mediaenc":    {MediaEnc: "srtp"},
		"advanced.extra":       {Extra: "answermode=auto"},
		"advanced.stunuser":    {StunUser: strings.Repeat("u", 129)},
	} {
		in := input()
		in.Advanced = &adv
		_, err := s.Create(ctx, anna, in)
		var v sipaccount.ValidationError
		if !errors.As(err, &v) || v[name] == "" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Octets, not characters: seven umlauts are fourteen octets, eight are sixteen.
	for n, ok := range map[int]bool{7: true, 8: false} {
		in := input()
		in.Advanced = &sipaccount.Advanced{LineName: strings.Repeat("ä", n)}
		if _, err := s.Create(ctx, anna, in); (err == nil) != ok {
			t.Errorf("%d umlauts: %v", n, err)
		}
	}
	in := input()
	in.Advanced = &sipaccount.Advanced{LineName: "123456789012345", Outbound2: "[2001:db8::1]:5060", StunServer: "stun.example.net", Extra: ";a=b;c_d=e"}
	if _, err := s.Create(ctx, anna, in); err != nil {
		t.Errorf("valid options refused: %v", err)
	}
}

// The mapping from the page's menus to the columns, and an account that never
// heard of the options.
func TestProfileChoices(t *testing.T) {
	for _, c := range []struct{ choice, col, extra string }{{"", "off", ""}, {"srtp", "optional", ""}, {"srtp-mand", "mandatory", ""},
		{"srtp-mandf", "mandatory", "srtp-mandf"}, {"dtls_srtp", "mandatory", "dtls_srtp"}} {
		col, extra, ok := sipaccount.EncColumn(c.choice)
		if !ok || col != c.col || extra != c.extra {
			t.Errorf("%q: %q %q %v", c.choice, col, extra, ok)
		}
		r := &sipaccount.Record{MediaEncryption: col, Advanced: sipaccount.Advanced{MediaEnc: extra}}
		if r.EncChoice() != c.choice {
			t.Errorf("%q comes back as %q", c.choice, r.EncChoice())
		}
	}
	if _, _, ok := sipaccount.EncColumn("rot13"); ok {
		t.Error("an unknown choice was mapped")
	}
	// Older accounts: autodetect where the settings are the defaults.
	old := &sipaccount.Record{MediaEncryption: "optional", Codecs: sipaccount.DefaultCodecs, DTMFMode: "rfc4733"}
	if !old.MediaAutoOn() || !old.EncAutoOn() || !old.Advanced.TransferOn() || old.DTMFChoice() != "rfc4733" {
		t.Error("an older default account does not read as autodetect")
	}
	old.Codecs, old.DTMFMode, old.MediaEncryption = []string{"PCMA"}, "inband", "off"
	if old.MediaAutoOn() || old.EncAutoOn() || old.DTMFChoice() != "inband" || old.EncChoice() != "" {
		t.Error("an older tuned account reads as autodetect")
	}
	if (sipaccount.Advanced{LegacySTUNServer: "stun.example.org"}).StunServerShown() != "stun.example.org" {
		t.Error("stun_server is not shown")
	}
}

func TestUses(t *testing.T) {
	s, admin, anna, ben, _ := setup(t)
	a, _ := s.Create(ctx, anna, input())
	var h1, h2 uuid.UUID
	s.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x01', 1, '\\x', 'Kitchen') RETURNING id", anna.ID).Scan(&h1)
	s.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, model) VALUES ($1, '\\x02', 2, '\\x', 'A540') RETURNING id", anna.ID).Scan(&h2)
	s.Pool.Exec(ctx, "INSERT INTO handset_line (handset_id, line, sip_account_id) VALUES ($1, 1, $3), ($2, 2, $3)", h1, h2, a.ID)
	u, err := s.Uses(ctx, anna, []uuid.UUID{a.ID})
	if err != nil || len(u[a.ID]) != 2 || u[a.ID][0].Handset != "A540" || u[a.ID][0].Line != 2 || u[a.ID][1].Handset != "Kitchen" {
		t.Fatalf("uses: %+v %v", u, err)
	}
	// Lines are private to the handset's owner.
	for _, who := range []*auth.User{ben, admin} {
		if u, err := s.Uses(ctx, who, []uuid.UUID{a.ID}); err != nil || len(u) != 0 {
			t.Errorf("%s sees %+v %v", who.Username, u, err)
		}
	}
	if u, err := s.Uses(ctx, anna, nil); err != nil || len(u) != 0 {
		t.Errorf("no ids: %+v %v", u, err)
	}
}
