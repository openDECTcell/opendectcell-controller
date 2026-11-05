// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/settings"
)

// stepTicked reports whether the overview's getting-started row with this text is marked done.
func stepTicked(t *testing.T, home, text string) bool {
	t.Helper()
	i := strings.Index(home, text)
	if i < 0 {
		t.Fatalf("the overview has no step %q", text)
	}
	return strings.Contains(home[strings.LastIndex(home[:i], "me-3 "):i], "text-success")
}

// The first administrator of a new controller is proposed a DECT network identity right after signing in; the page
// tells what accepting and declining mean.
func TestFirstRunProposesAnIdentity(t *testing.T) {
	e, _, _ := newOpsEnv(t)
	a, u := e.adminBrowser(t), e.userBrowser(t)

	if r, _ := e.get(t, a, "/"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/setup/identity" {
		t.Fatalf("admin's first page: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := e.get(t, u, "/"); r.StatusCode != http.StatusOK {
		t.Errorf("a user is not proposed anything: %d", r.StatusCode)
	}
	if r, _ := e.get(t, u, "/setup/identity"); r.StatusCode != http.StatusNotFound {
		t.Errorf("a user opened the step: %d", r.StatusCode)
	}
	r, page := e.get(t, a, "/setup/identity")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("step: %d", r.StatusCode)
	}
	// the hints: what the proposal is, what accepting costs later, what saying no means
	for _, want := range []string{"Proposed identity", "made up on this controller", "may belong to a real DECT system",
		"Changing it later makes every handset register again", "A540", "If you say no:", "a DECT system of its own",
		"warning until an identity is set", "I have an EIC from ETSI"} {
		if !strings.Contains(page, want) {
			t.Errorf("the step lacks the hint %q", want)
		}
	}

	// accepting needs the tick, a valid identity
	form := url.Values{"action": {"accept"}, "eic": {"3A7F"}, "fpn": {"12"}, "fps": {"5"}, "source": {"random"}}
	if r := e.post(t, a, "/setup/identity", form); r.StatusCode != http.StatusBadRequest {
		t.Errorf("accepted without the tick: %d", r.StatusCode)
	}
	if id, _ := e.cells.SystemIdentity(ctx); id != nil {
		t.Fatal("an identity was saved without the tick")
	}
	bad := url.Values{"action": {"accept"}, "eic": {"0000"}, "fpn": {"12"}, "fps": {"5"}, "source": {"random"}, "understood": {"on"}}
	if r := e.post(t, a, "/setup/identity", bad); r.StatusCode != http.StatusBadRequest {
		t.Errorf("accepted EIC 0: %d", r.StatusCode)
	}
	if r := e.post(t, u, "/setup/identity", url.Values{"action": {"decline"}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("a user declined for the administrators: %d", r.StatusCode)
	}

	form.Set("understood", "on")
	if r := e.post(t, a, "/setup/identity", form); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("accept: %d", r.StatusCode)
	}
	id, err := e.cells.SystemIdentity(ctx)
	if err != nil || id == nil || id.EIC != 0x3A7F || id.FPN != 12 || id.FPS != 5 || id.Source != "random" || !id.Apply || id.SetBy != "admin" {
		t.Fatalf("saved identity: %+v %v", id, err)
	}
	var n int
	if err := e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action = 'dect_identity.set'").Scan(&n); err != nil || n != 1 {
		t.Errorf("audit of the identity: %d %v", n, err)
	}
	// done: the overview opens, with the step ticked and no warning
	r, home := e.get(t, a, "/")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("overview after the step: %d", r.StatusCode)
	}
	if strings.Contains(home, "has no identity") {
		t.Error("the warning stays after an identity was set")
	}
	if !stepTicked(t, home, "The DECT network has an identity") {
		t.Error("the getting-started step is not ticked after the identity was set")
	}
	// the step is over: nothing it offers can replace an identity that exists
	other := url.Values{"action": {"accept"}, "eic": {"1111"}, "fpn": {"1"}, "fps": {"1"}, "source": {"random"}, "understood": {"on"}}
	if r := e.post(t, a, "/setup/identity", other); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/network-identity" {
		t.Errorf("the step after it was done: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if id2, _ := e.cells.SystemIdentity(ctx); id2 == nil || id2.EIC != 0x3A7F {
		t.Errorf("the step replaced the identity: %+v", id2)
	}
	if r, _ := e.get(t, a, "/setup/identity"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/network-identity" {
		t.Errorf("the step page after it was done: %d", r.StatusCode)
	}
}

// Saying no is allowed; the step goes away, the identity stays unset, and the overview keeps saying what it means.
func TestFirstRunDecline(t *testing.T) {
	e, _, _ := newOpsEnv(t)
	a := e.adminBrowser(t)
	if r := e.post(t, a, "/setup/identity", url.Values{"action": {"decline"}}); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("decline: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if id, _ := e.cells.SystemIdentity(ctx); id != nil {
		t.Errorf("declining set an identity: %+v", id)
	}
	var set settings.Setup
	if ok, err := e.app.Settings.Get(ctx, settings.KeySetup, &set); err != nil || !ok || !set.IdentityDeclined || set.IdentityDeclinedBy != "admin" || set.IdentityDeclinedAt.IsZero() {
		t.Errorf("remembered: %+v %v %v", set, ok, err)
	}
	var n int
	if err := e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action = 'setup.identity_declined'").Scan(&n); err != nil || n != 1 {
		t.Errorf("audit of declining: %d %v", n, err)
	}
	r, home := e.get(t, a, "/")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("overview after declining: %d (the step must not come back)", r.StatusCode)
	}
	for _, want := range []string{"The DECT network has no identity", "every base station is a DECT system of its own", "Set an identity", "/network-identity"} {
		if !strings.Contains(home, want) {
			t.Errorf("the overview lacks %q", want)
		}
	}
	if stepTicked(t, home, "The DECT network has an identity") {
		t.Error("the getting-started step is ticked without an identity")
	}
	if r, _ := e.get(t, a, "/setup/identity"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/network-identity" {
		t.Errorf("the step page after declining: %d", r.StatusCode)
	}
	// the Network identity page can still set one
	if r := e.post(t, a, "/network-identity", url.Values{"action": {"random"}}); r.StatusCode != http.StatusOK {
		t.Errorf("the identity page after declining: %d", r.StatusCode)
	}
}

// With a handset registered, setting an identity makes it register again: the first-run step does not offer it, the
// Network identity page (which asks) does, and the overview warns.
func TestFirstRunNotWithHandsets(t *testing.T) {
	e, _, _ := newOpsEnv(t)
	a := e.adminBrowser(t)
	var owner uuid.UUID
	if err := e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Pool.Exec(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\\x0102030405', 17, '\\x00')", owner); err != nil {
		t.Fatal(err)
	}
	r, home := e.get(t, a, "/")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("overview with a handset: %d", r.StatusCode)
	}
	if !strings.Contains(home, "The DECT network has no identity") {
		t.Error("no warning on a controller with handsets and no identity")
	}
	acc := url.Values{"action": {"accept"}, "eic": {"3A7F"}, "fpn": {"1"}, "fps": {"1"}, "source": {"random"}, "understood": {"on"}}
	if r := e.post(t, a, "/setup/identity", acc); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/network-identity" {
		t.Errorf("the step with a handset registered: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if id, _ := e.cells.SystemIdentity(ctx); id != nil {
		t.Errorf("an identity was set under a registered handset: %+v", id)
	}
}

// What the identity's source is recorded as: the made-up one says so, anything else is an allocated EIC.
func TestFirstRunSource(t *testing.T) {
	for _, c := range []struct{ source, want string }{{"random", "random"}, {"allocated", "allocated"}, {"nonsense", "allocated"}, {"", "allocated"}} {
		e, _, _ := newOpsEnv(t)
		a := e.adminBrowser(t)
		form := url.Values{"action": {"accept"}, "eic": {"5C3D"}, "fpn": {"7"}, "fps": {"3"}, "source": {c.source}, "understood": {"on"}}
		if r := e.post(t, a, "/setup/identity", form); r.StatusCode != http.StatusSeeOther {
			t.Fatalf("accept with source %q: %d", c.source, r.StatusCode)
		}
		if id, err := e.cells.SystemIdentity(ctx); err != nil || id == nil || id.Source != c.want {
			t.Errorf("source %q recorded as %+v %v, want %q", c.source, id, err, c.want)
		}
	}
}

// "Propose another" and the typed EIC only show a proposal; nothing is saved until it is accepted.
func TestFirstRunProposals(t *testing.T) {
	e, _, _ := newOpsEnv(t)
	a := e.adminBrowser(t)
	r, page := e.get(t, a, "/setup/identity")
	if r.StatusCode != http.StatusOK || !strings.Contains(page, "Propose another") {
		t.Fatalf("step: %d", r.StatusCode)
	}
	if r := e.post(t, a, "/setup/identity", url.Values{"action": {"another"}}); r.StatusCode != http.StatusOK {
		t.Errorf("another: %d", r.StatusCode)
	}
	resp := e.post(t, a, "/setup/identity", url.Values{"action": {"eic"}, "eic": {"4B2C"}, "fpn": {"3"}, "fps": {"2"}})
	body := readBody(resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "an EIC allocated by ETSI") || !strings.Contains(body, `value="4B2C"`) || strings.Contains(body, "Propose another") {
		t.Errorf("a typed EIC: %d", resp.StatusCode)
	}
	if r := e.post(t, a, "/setup/identity", url.Values{"action": {"eic"}, "eic": {"zz"}, "fpn": {"3"}, "fps": {"2"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a bad EIC: %d", r.StatusCode)
	}
	if r := e.post(t, a, "/setup/identity", url.Values{"action": {"nonsense"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown action: %d", r.StatusCode)
	}
	if id, _ := e.cells.SystemIdentity(ctx); id != nil {
		t.Errorf("a proposal was saved: %+v", id)
	}
	if ok, _ := e.app.Settings.Get(ctx, settings.KeySetup, &settings.Setup{}); ok {
		t.Error("a proposal was remembered as declined")
	}
}
