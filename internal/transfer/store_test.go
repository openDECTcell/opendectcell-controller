// SPDX-License-Identifier: AGPL-3.0-or-later

package transfer_test

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/core"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
	"github.com/openDECTcell/opendectcell-controller/internal/numberplan"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
	"github.com/openDECTcell/opendectcell-controller/internal/transfer"
)

var ctx = context.Background()

func ptr[T any](v T) *T { return &v }

type env struct {
	s          *transfer.Store
	core       *core.Store
	lines      *sipaccount.Store
	contacts   *contacts.Store
	handsets   *handset.Store
	anna, ben  *auth.User
	cell       uuid.UUID
	annaHandID uuid.UUID
}

const uak = "00112233445566778899aabbccddeeff"

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.Migrated(t)
	ks, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(ks)
	kr, err := db.LoadKeyring(ctx, pool, mk, nil)
	if err != nil {
		t.Fatal(err)
	}
	users := &auth.Store{Pool: pool, Argon: auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1}, SessionIdle: time.Hour, SessionMaxAge: time.Hour, RateLimit: 5}
	mkUser := func(name string) *auth.User {
		u, err := users.CreateLocalUser(ctx, auth.NewLocalUser{Username: name, Password: "long password", Role: auth.RoleUser})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	e := &env{core: &core.Store{Pool: pool, Keyring: kr}, lines: &sipaccount.Store{Pool: pool, Keyring: kr},
		contacts: &contacts.Store{Pool: pool}, handsets: &handset.Store{Pool: pool}, anna: mkUser("anna"), ben: mkUser("ben")}
	e.s = &transfer.Store{Pool: pool, Records: e.core, Lines: e.lines, Contacts: e.contacts, Handsets: e.handsets}
	if err := pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('c', 'active') RETURNING id").Scan(&e.cell); err != nil {
		t.Fatal(err)
	}
	return e
}

// seed gives anna two lines (one with a TURN password), a handset enrolled the normal way with a known key, and a contact.
func (e *env) seed(t *testing.T) {
	t.Helper()
	actx := audit.WithActor(ctx, e.anna.Actor())
	home, err := e.lines.Create(actx, e.anna, sipaccount.Input{Label: ptr("Home"), Username: ptr("4930"), Domain: ptr("sip.example.net"),
		Password: ptr("pw-home-1"), StunPass: ptr("turn-pw-1"), Transport: ptr("udp"), MediaEncryption: ptr("off"),
		Advanced: &sipaccount.Advanced{LineName: "Home", Numbers: numbers()}})
	if err != nil {
		t.Fatal(err)
	}
	office, err := e.lines.Create(actx, e.anna, sipaccount.Input{Label: ptr("Office"), Username: ptr("200"), Domain: ptr("pbx.example.net"),
		Password: ptr("pw-office"), Transport: ptr("tcp"), MediaEncryption: ptr("off")})
	if err != nil {
		t.Fatal(err)
	}
	r := stack.Record{Slot: 1, PortID: []byte{0x80, 0xa8, 0x03, 0x09}, CCPortID: []byte{0x80, 0xa8, 0x03, 0x09}, CCFixedID: []byte{0, 1, 2, 3, 4}}
	b, _ := hex.DecodeString(uak)
	copy(r.UAK[:], b)
	sv, err := e.core.SaveRecord(ctx, e.cell, uuid.Nil, e.anna.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	e.annaHandID = sv.ID
	two := 2
	if err := e.handsets.Update(actx, e.anna, sv.ID, handset.Update{Name: "Hall", DefaultLine: &two, DualRing: "busy",
		Lines: map[int]uuid.UUID{1: home.ID, 2: office.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.contacts.Create(actx, e.anna, contacts.Contact{LastName: "Meier", FirstName: "Eva",
		Numbers: []contacts.Number{{Number: "+4930123456", Kind: "work", Default: true}}}, false); err != nil {
		t.Fatal(err)
	}
}

func (e *env) key(t *testing.T, owner, id uuid.UUID) string {
	t.Helper()
	k, err := e.core.ExportKey(ctx, owner, id)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(k.UAK[:])
}

func (e *env) handsetOf(t *testing.T, u *auth.User) []*handset.Handset {
	t.Helper()
	hs, err := e.handsets.List(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	return hs
}

func all() transfer.Options {
	return transfer.Options{Handsets: true, Lines: true, Contacts: true, Secrets: true}
}

func TestExportHoldsOnlyTheUsersOwnAndOnlyWhatWasAsked(t *testing.T) {
	e := newEnv(t)
	e.seed(t)
	set, err := e.s.Export(ctx, e.anna, all())
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Handsets) != 1 || len(set.Lines) != 2 || set.Handsets[0].Key == nil || set.Handsets[0].Key.UAK != uak ||
		set.Handsets[0].Key.CCFixedID != "0001020304" || !strings.Contains(set.Contacts, "Meier") {
		t.Fatalf("export: %+v", set)
	}
	pw := map[string]string{}
	for _, l := range set.Lines {
		pw[l.Label] = l.Password
	}
	if pw["Home"] != "pw-home-1" || pw["Office"] != "pw-office" || set.Lines[0].StunPassword+set.Lines[1].StunPassword != "turn-pw-1" {
		t.Errorf("line secrets: %v", pw)
	}
	if set.Handsets[0].Lines["1"] == "" || set.Handsets[0].Lines["1"] == set.Handsets[0].Lines["2"] {
		t.Errorf("handset lines: %v", set.Handsets[0].Lines)
	}
	// without secrets nothing secret is in it, and it can be written plain
	set, err = e.s.Export(ctx, e.anna, transfer.Options{Handsets: true, Lines: true})
	if err != nil || set.HasSecrets() || set.Contacts != "" {
		t.Fatalf("export without secrets: %v %+v", err, set)
	}
	if _, err := transfer.Plain(set); err != nil {
		t.Error(err)
	}
	// another user exports nothing of anna's
	set, err = e.s.Export(ctx, e.ben, all())
	if err != nil || len(set.Handsets)+len(set.Lines) != 0 || strings.Contains(set.Contacts, "Meier") {
		t.Fatalf("ben's export: %v %+v", err, set)
	}
}

// The move to another controller: the handset is gone from here, the file brings it back with key, lines and passwords.
func TestImportBringsEverythingBack(t *testing.T) {
	e := newEnv(t)
	e.seed(t)
	set, _ := e.s.Export(ctx, e.anna, all())
	file, err := transfer.Seal(set, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.handsets.Delete(audit.WithActor(ctx, e.anna.Actor()), e.anna, e.annaHandID); err != nil {
		t.Fatal(err)
	}
	got, _, err := transfer.Open(file, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	// a dry run writes nothing
	rep, err := e.s.Import(ctx, e.ben, got, true)
	if err != nil || !rep.DryRun || rep.Added != 4 || rep.Failed != 0 {
		t.Fatalf("dry run: %v %+v", err, rep)
	}
	if n := len(e.handsetOf(t, e.ben)); n != 0 {
		t.Fatalf("a dry run made %d handsets", n)
	}
	if recs, _ := e.lines.List(ctx, e.ben, ""); len(recs) != 0 {
		t.Fatalf("a dry run made %d lines", len(recs))
	}
	bctx := audit.WithActor(ctx, e.ben.Actor())
	rep, err = e.s.Import(bctx, e.ben, got, false)
	if err != nil || rep.Added != 4 || rep.Failed != 0 || len(rep.ChangedHandsets) != 1 {
		t.Fatalf("import: %v %+v", err, rep)
	}
	hs := e.handsetOf(t, e.ben)
	if len(hs) != 1 || hs[0].Name != "Hall" || hs[0].DualRing != "busy" || hs[0].DefaultLine == nil || *hs[0].DefaultLine != 2 || len(hs[0].Lines) != 2 {
		t.Fatalf("handset: %+v", hs)
	}
	if k := e.key(t, e.ben.ID, hs[0].ID); k != uak {
		t.Errorf("the key did not survive: %s", k)
	}
	if hs[0].CellsHoldingN != 0 || hs[0].TPUI == 0 {
		t.Errorf("a new handset holds no slot yet and has its own TPUI: %+v", hs[0])
	}
	recs, _ := e.lines.List(ctx, e.ben, "")
	pws := map[string]string{}
	for _, r := range recs {
		p, st, err := e.lines.Secrets(ctx, e.ben, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		pws[r.Label] = p + "/" + st
		if r.Label == "Home" && (r.Advanced.LineName != "Home" || r.Advanced.Numbers.CountryCode != "49") {
			t.Errorf("advanced settings lost: %+v", r.Advanced)
		}
	}
	if pws["Home"] != "pw-home-1/turn-pw-1" || pws["Office"] != "pw-office/" {
		t.Errorf("passwords: %v", pws)
	}
	if cs, _ := e.contacts.List(ctx, e.ben, "", ""); len(cs) != 1 || cs[0].LastName != "Meier" {
		t.Errorf("contacts: %+v", cs)
	}
	// the same file again changes nothing: no second handset, no second line, no second contact
	rep, err = e.s.Import(bctx, e.ben, got, false)
	if err != nil || rep.Failed != 0 || len(e.handsetOf(t, e.ben)) != 1 {
		t.Fatalf("second import: %v %+v", err, rep)
	}
	if recs, _ := e.lines.List(ctx, e.ben, ""); len(recs) != 2 {
		t.Errorf("%d lines after a second import", len(recs))
	}
	if cs, _ := e.contacts.List(ctx, e.ben, "", ""); len(cs) != 1 {
		t.Errorf("%d contacts after a second import", len(cs))
	}
}

// A handset that is registered here, by anyone, is never taken over by a file; its key is not replaced.
func TestImportNeverTakesAHandsetThatIsHere(t *testing.T) {
	e := newEnv(t)
	e.seed(t)
	set, _ := e.s.Export(ctx, e.anna, all())
	// ben imports anna's file while her handset is still registered
	rep, err := e.s.Import(audit.WithActor(ctx, e.ben.Actor()), e.ben, set, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.handsetOf(t, e.ben)); n != 0 {
		t.Fatalf("ben took the handset: %d", n)
	}
	var skipped bool
	for _, it := range rep.Items {
		if it.Kind == "handset" {
			skipped = it.Action == "skip"
			if strings.Contains(it.Note, "anna") || strings.Contains(it.Note, uak) {
				t.Errorf("the note says too much: %q", it.Note)
			}
		}
	}
	if !skipped {
		t.Errorf("report: %+v", rep.Items)
	}
	if k := e.key(t, e.anna.ID, e.annaHandID); k != uak {
		t.Errorf("anna's key changed: %s", k)
	}
}

// A file without keys brings settings to the handset the user already has, and creates no handset.
func TestImportSettingsOnlyUpdatesOwnHandset(t *testing.T) {
	e := newEnv(t)
	e.seed(t)
	set, _ := e.s.Export(ctx, e.anna, transfer.Options{Handsets: true, Lines: true})
	set.Handsets[0].Name = "Renamed"
	set.Handsets[0].DualRing = "call_waiting"
	set.Handsets = append(set.Handsets, transfer.Handset{Name: "Stranger", IPUI: "7777"})
	rep, err := e.s.Import(audit.WithActor(ctx, e.anna.Actor()), e.anna, set, false)
	if err != nil || rep.Failed != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
	hs := e.handsetOf(t, e.anna)
	if len(hs) != 1 || hs[0].Name != "Renamed" || hs[0].DualRing != "call_waiting" || len(hs[0].Lines) != 2 {
		t.Fatalf("handsets: %+v", hs)
	}
	if k := e.key(t, e.anna.ID, e.annaHandID); k != uak {
		t.Errorf("the key changed: %s", k)
	}
	var stranger transfer.Item
	for _, it := range rep.Items {
		if it.Name == "Stranger" {
			stranger = it
		}
	}
	if stranger.Action != "skip" {
		t.Errorf("a handset without a key was %q", stranger.Action)
	}
}

func TestImportReportsBadItemsAndGoesOn(t *testing.T) {
	e := newEnv(t)
	set := transfer.Setup{
		Lines: []transfer.Line{
			{Ref: "l1", Label: "Bad", Username: "x", Domain: "d.example", Transport: "carrier-pigeon", Keepalive: "auto", MediaEncryption: "off", DTMFMode: "rfc4733", CallerIDSource: "auto", RegisterExpiry: 600},
			{Ref: "l2", Label: "Good", Username: "y", Domain: "d.example", Transport: "udp", Keepalive: "auto", MediaEncryption: "off", DTMFMode: "rfc4733", CallerIDSource: "auto", RegisterExpiry: 600, Enabled: true},
		},
		Handsets: []transfer.Handset{
			{Name: "NoHex", IPUI: "zz", Key: &transfer.Key{UAK: uak}},
			{Name: "ShortKey", IPUI: "0a", Key: &transfer.Key{UAK: "0011"}},
			{Name: "Fine", IPUI: "0b", Key: &transfer.Key{UAK: uak}, Lines: map[string]string{"1": "l1", "2": "l2"}, DefaultLine: ptr(1)},
		},
	}
	rep, err := e.s.Import(audit.WithActor(ctx, e.ben.Actor()), e.ben, set, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range rep.Items {
		got[it.Name] = it.Action
	}
	if got["Bad"] != "fail" || got["Good"] != "add" || got["NoHex"] != "fail" || got["ShortKey"] != "fail" || got["Fine"] != "add" {
		t.Fatalf("actions: %v\n%+v", got, rep.Items)
	}
	hs := e.handsetOf(t, e.ben)
	if len(hs) != 1 || len(hs[0].Lines) != 1 || hs[0].Lines[0].Line != 2 {
		t.Fatalf("a line that was not made must not be set: %+v", hs)
	}
	// and the audit says counts, never values
	var n int
	if err := e.s.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action = 'setup.import' AND details::text LIKE '%'||$1||'%'", uak).Scan(&n); err != nil || n != 0 {
		t.Errorf("the audit holds the key (%d, %v)", n, err)
	}
}

func numbers() numberplan.Rules { return numberplan.Rules{CountryCode: "49", AreaCode: "30"} }

// A shared line belongs to the administrators' whole instance, not to the admin who exports: it does not leave in their file.
func TestExportLeavesSharedLinesOut(t *testing.T) {
	e := newEnv(t)
	e.seed(t)
	users := &auth.Store{Pool: e.s.Pool, Argon: auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1}, SessionIdle: time.Hour, SessionMaxAge: time.Hour, RateLimit: 5}
	admin, err := users.CreateLocalUser(ctx, auth.NewLocalUser{Username: "root", Password: "long password", Role: auth.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	var none *uuid.UUID
	if _, err := e.lines.Create(audit.WithActor(ctx, admin.Actor()), admin, sipaccount.Input{OwnerID: &none, Label: ptr("Shared"), Username: ptr("1"), Domain: ptr("pbx.example.net"),
		Password: ptr("shared-pw"), Transport: ptr("udp"), MediaEncryption: ptr("off")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.lines.Create(audit.WithActor(ctx, admin.Actor()), admin, sipaccount.Input{Label: ptr("Mine"), Username: ptr("2"), Domain: ptr("pbx.example.net"),
		Password: ptr("my-pw"), Transport: ptr("udp"), MediaEncryption: ptr("off")}); err != nil {
		t.Fatal(err)
	}
	set, err := e.s.Export(ctx, admin, all())
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Lines) != 1 || set.Lines[0].Label != "Mine" {
		t.Fatalf("the admin's export: %+v", set.Lines)
	}
}

func TestSecretsOfAnotherUsersLineAreNotGiven(t *testing.T) {
	e := newEnv(t)
	e.seed(t)
	recs, _ := e.lines.List(ctx, e.anna, "")
	if _, _, err := e.lines.Secrets(ctx, e.ben, recs[0].ID); err == nil {
		t.Fatal("ben read anna's password")
	}
	if p, _, err := e.lines.Secrets(ctx, e.anna, recs[0].ID); err != nil || p == "" {
		t.Fatalf("anna reads her own: %q %v", p, err)
	}
}
