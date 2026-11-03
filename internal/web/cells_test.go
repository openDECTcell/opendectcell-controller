// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/pki"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
)

type fakeLink struct {
	mu     sync.Mutex
	states map[uuid.UUID]wire.CellStateValue
	kicked []uuid.UUID
}

func (f *fakeLink) CellStatus(uuid.UUID) cell.Status { return cell.Status{} }
func (f *fakeLink) NotifyState(id uuid.UUID, st wire.CellStateValue, _ string) {
	f.mu.Lock()
	f.states[id] = st
	f.mu.Unlock()
}
func (f *fakeLink) Kick(id uuid.UUID) { f.mu.Lock(); f.kicked = append(f.kicked, id); f.mu.Unlock() }

var enrollRE = regexp.MustCompile(`opendectcell-enroll:v1\?[^<\s]+`)

func TestCellPages(t *testing.T) {
	link := &fakeLink{states: map[uuid.UUID]wire.CellStateValue{}}
	e := newEnvWith(t, func(s *Server) {
		mk := testMK
		kr, err := db.LoadKeyring(ctx, s.Pool, mk, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Cells, err = cell.Open(ctx, s.Pool, kr, "dect.example.org:7300"); err != nil {
			t.Fatal(err)
		}
		s.CellLink = link
	})
	admin := e.browser()
	e.login(t, admin, "admin", "admin password")

	// Create a token: the enrollment string appears once.
	r := e.post(t, admin, "/enroll", url.Values{"note": {"Hall"}, "ttl": {"1h"}, "auto_approve": {""}})
	body := readBody(r)
	m := enrollRE.FindString(html.UnescapeString(body))
	if r.StatusCode != http.StatusOK || m == "" {
		t.Fatalf("create token: %d", r.StatusCode)
	}
	en, err := pki.ParseEnrollment(m)
	if err != nil || en.Addr != "dect.example.org:7300" {
		t.Fatalf("enrollment string %q: %v", m, err)
	}
	if _, page := e.get(t, admin, "/enroll"); enrollRE.MatchString(html.UnescapeString(page)) {
		t.Fatal("enrollment string shown again")
	}

	// Enroll through the store (the TLS path is tested in internal/cell).
	res, err := e.cells.Enroll(ctx, wire.EnrollRequestMsg{Token: en.Token, CSR: testCSR(t), SWVersion: "v"}, "")
	if err != nil {
		t.Fatal(err)
	}
	id := res.CellID.String()
	if _, page := e.get(t, admin, "/cells"); !strings.Contains(page, "Hall") || !strings.Contains(page, "pending") {
		t.Fatal("cell list lacks the pending cell")
	}
	if r := e.post(t, admin, "/cells/"+id+"/state", url.Values{"state": {"active"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve: %d", r.StatusCode)
	}
	if link.states[res.CellID] != wire.StateActive {
		t.Error("approval not pushed to the link")
	}
	// Revoking is the owner's: it lives on the station page, and somebody else's base station is not found.
	user := e.browser()
	e.login(t, user, "user", "user password")
	if r := e.post(t, user, "/stations/"+id+"/revoke", url.Values{"confirm": {"Hall"}}); r.StatusCode != http.StatusNotFound || len(link.kicked) != 0 {
		t.Fatalf("revoke by somebody else: %d", r.StatusCode)
	}
	if r := e.post(t, admin, "/cells/"+id+"/revoke", url.Values{"confirm": {"Hall"}}); r.StatusCode != http.StatusNotFound && r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("the admin cell page still revokes: %d", r.StatusCode)
	}
	if r := e.post(t, admin, "/stations/"+id+"/revoke", url.Values{"confirm": {"wrong"}}); r.StatusCode != http.StatusBadRequest || len(link.kicked) != 0 {
		t.Fatalf("revoke without confirmation: %d", r.StatusCode)
	}
	if r := e.post(t, admin, "/stations/"+id+"/revoke", url.Values{"confirm": {"Hall"}}); r.StatusCode != http.StatusSeeOther || len(link.kicked) != 1 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}

	// A plain user sees neither page; with may_add_cells, only /enroll.
	for _, p := range []string{"/enroll", "/cells", "/cells/" + id} {
		if resp, _ := e.get(t, user, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("user on %s: %d", p, resp.StatusCode)
		}
	}
	e.store.Pool.Exec(ctx, "UPDATE app_user SET may_add_cells = true WHERE username = 'user'")
	if resp, _ := e.get(t, user, "/enroll"); resp.StatusCode != http.StatusOK {
		t.Errorf("may_add_cells user on /enroll: %d", resp.StatusCode)
	}
	if r := e.post(t, user, "/enroll", url.Values{"auto_approve": {"on"}}); r.StatusCode != http.StatusForbidden {
		t.Errorf("user creating an auto-approve token: %d", r.StatusCode)
	}
	if resp, _ := e.get(t, user, "/cells"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("may_add_cells user on /cells: %d", resp.StatusCode)
	}
}

var hiddenRE = regexp.MustCompile(`name="(eic|fpn|fps|source)" value="([^"]*)"`)

// The system identity on the Cells page: made up only after the admin has
// seen the risks and ticked the box; an EIC typed in is checked.
func TestSystemIdentityPages(t *testing.T) {
	e := newEnvWith(t, func(s *Server) {
		mk := testMK
		kr, err := db.LoadKeyring(ctx, s.Pool, mk, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Cells, err = cell.Open(ctx, s.Pool, kr, "dect.example.org:7300"); err != nil {
			t.Fatal(err)
		}
	})
	admin := e.browser()
	e.login(t, admin, "admin", "admin password")
	if _, page := e.get(t, admin, "/network-identity"); !strings.Contains(page, "factory identity") {
		t.Fatal("no identity: the page does not say so")
	}

	r := e.post(t, admin, "/network-identity", url.Values{"action": {"random"}})
	page := readBody(r)
	if r.StatusCode != http.StatusOK || !strings.Contains(page, "This EIC is made up") ||
		!strings.Contains(page, "https://www.etsi.org/security-algorithms-and-codes/codes") {
		t.Fatalf("proposal page: %d", r.StatusCode)
	}
	form := url.Values{}
	for _, m := range hiddenRE.FindAllStringSubmatch(page, -1) {
		form.Set(m[1], m[2])
	}
	if form.Get("source") != cell.IdentityRandom || len(form.Get("eic")) != 4 {
		t.Fatalf("hidden fields %v", form)
	}
	if r := e.post(t, admin, "/network-identity/save", form); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("saved without the box ticked: %d", r.StatusCode)
	}
	if id, _ := e.cells.SystemIdentity(ctx); id != nil {
		t.Fatal("stored without the box ticked")
	}
	form.Set("understood", "on")
	if r := e.post(t, admin, "/network-identity/save", form); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", r.StatusCode)
	}
	id, err := e.cells.SystemIdentity(ctx)
	if err != nil || id == nil || id.Source != cell.IdentityRandom || fmt.Sprintf("%04X", id.EIC) != form.Get("eic") {
		t.Fatalf("stored %+v, %v", id, err)
	}
	if _, page := e.get(t, admin, "/network-identity"); !strings.Contains(page, "made up on this controller") {
		t.Fatal("the list does not show the identity")
	}

	// An EIC typed in: EIC 0 and FPS 0 are refused (EN 300 175-6 §5.2).
	for _, bad := range []url.Values{
		{"eic": {"0000"}, "fpn": {"1"}, "fps": {"1"}},
		{"eic": {"12G4"}, "fpn": {"1"}, "fps": {"1"}},
		{"eic": {"1234"}, "fpn": {"1"}, "fps": {"0"}},
	} {
		if r := e.post(t, admin, "/network-identity", bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, r.StatusCode)
		}
	}
	r = e.post(t, admin, "/network-identity", url.Values{"eic": {"beef"}, "fpn": {"7"}, "fps": {"2"}})
	if page = readBody(r); r.StatusCode != http.StatusOK || strings.Contains(page, "This EIC is made up") ||
		!strings.Contains(page, `value="BEEF"`) {
		t.Fatalf("allocated EIC: %d", r.StatusCode)
	}

	// Replacing a stored identity asks for more: both boxes and the old EIC.
	if !strings.Contains(page, "This replaces the identity of a running system") {
		t.Error("the proposal does not warn that it replaces the identity")
	}
	rep := url.Values{"eic": {"BEEF"}, "fpn": {"7"}, "fps": {"2"}, "source": {"allocated"}, "understood": {"on"}}
	if r := e.post(t, admin, "/network-identity/save", rep); r.StatusCode != http.StatusBadRequest {
		t.Errorf("replaced with one box: %d", r.StatusCode)
	}
	rep.Set("understood2", "on")
	if r := e.post(t, admin, "/network-identity/save", rep); r.StatusCode != http.StatusBadRequest {
		t.Errorf("replaced without typing the old EIC: %d", r.StatusCode)
	}
	rep.Set("confirm_eic", "0000")
	if r := e.post(t, admin, "/network-identity/save", rep); r.StatusCode != http.StatusBadRequest {
		t.Errorf("replaced with the wrong EIC typed: %d", r.StatusCode)
	}
	if now, _ := e.cells.SystemIdentity(ctx); now == nil || fmt.Sprintf("%04X", now.EIC) != form.Get("eic") {
		t.Fatal("the identity changed without every confirmation")
	}
	rep.Set("confirm_eic", strings.ToLower(form.Get("eic")))
	if r := e.post(t, admin, "/network-identity/save", rep); r.StatusCode != http.StatusSeeOther {
		t.Errorf("replace with every confirmation: %d", r.StatusCode)
	}
	if now, _ := e.cells.SystemIdentity(ctx); now == nil || now.EIC != 0xBEEF {
		t.Errorf("not replaced: %+v", now)
	}

	// The cells use it only after both boxes are ticked.
	ap := url.Values{"apply": {"on"}, "understood": {"on"}}
	if r := e.post(t, admin, "/network-identity/apply", ap); r.StatusCode != http.StatusBadRequest {
		t.Errorf("applied with one box: %d", r.StatusCode)
	}
	if now, _ := e.cells.SystemIdentity(ctx); now == nil || now.Apply {
		t.Fatalf("applied with one box: %+v", now)
	}
	ap.Set("understood2", "on")
	if r := e.post(t, admin, "/network-identity/apply", ap); r.StatusCode != http.StatusSeeOther {
		t.Errorf("apply with both boxes: %d", r.StatusCode)
	}
	if now, _ := e.cells.SystemIdentity(ctx); now == nil || !now.Apply {
		t.Errorf("not applied: %+v", now)
	}
	// None of it is on the Cells page any more; the menu has its own entry.
	if _, page := e.get(t, admin, "/cells"); strings.Contains(page, "Use this identity on all cells") ||
		strings.Contains(page, "Replace the system identity") || !strings.Contains(page, `href="/network-identity"`) {
		t.Error("the Cells page still carries the identity buttons, or does not point to the new page")
	}

	// Not for users.
	user := e.browser()
	e.login(t, user, "user", "user password")
	if resp, _ := e.get(t, user, "/network-identity"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("user GET /network-identity: %d", resp.StatusCode)
	}
	for _, p := range []string{"/network-identity", "/network-identity/save", "/network-identity/apply", "/network-identity/donor"} {
		if r := e.post(t, user, p, url.Values{"action": {"random"}}); r.StatusCode != http.StatusNotFound {
			t.Errorf("user POST %s: %d", p, r.StatusCode)
		}
	}
}

// Who may enroll base stations is a setting: every user while it is on (the
// default), only admins and users ticked "may enroll" when an admin turns it off.
func TestEveryoneMayEnrollSetting(t *testing.T) {
	e := newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Cells, err = cell.Open(ctx, s.Pool, kr, "dect.example.org:7300"); err != nil {
			t.Fatal(err)
		}
		s.CellLink = &fakeLink{states: map[uuid.UUID]wire.CellStateValue{}}
	})
	e.store.EveryoneMayEnroll = func(ctx context.Context) bool {
		c, err := e.app.Settings.Cells(ctx)
		return err == nil && c.EveryoneMayEnroll
	}
	admin, user := e.adminBrowser(t), e.userBrowser(t)
	status := func(c *http.Client) int { r, _ := e.get(t, c, "/enroll"); return r.StatusCode }
	if got, _ := e.app.Settings.Cells(ctx); !got.EveryoneMayEnroll {
		t.Fatal("not on by default")
	}
	if s := status(user); s != http.StatusOK {
		t.Errorf("a plain user on /enroll with the setting on (the default): %d", s)
	}
	if _, pg := e.get(t, admin, "/admin/settings"); !strings.Contains(pg, `name="everyone_may_enroll" checked`) {
		t.Error("the settings page does not show it on")
	}
	if r := e.post(t, admin, "/admin/settings/cells", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("turn off: %d", r.StatusCode)
	}
	if s := status(user); s != http.StatusNotFound {
		t.Errorf("a plain user on /enroll with the setting off: %d", s)
	}
	if s := status(admin); s != http.StatusOK {
		t.Errorf("an admin on /enroll with the setting off: %d", s)
	}
	e.store.Pool.Exec(ctx, "UPDATE app_user SET may_add_cells = true WHERE username = 'user'")
	if s := status(user); s != http.StatusOK {
		t.Errorf("a ticked user on /enroll with the setting off: %d", s)
	}
	e.store.Pool.Exec(ctx, "UPDATE app_user SET may_add_cells = false WHERE username = 'user'")
	if r := e.post(t, user, "/admin/settings/cells", url.Values{"everyone_may_enroll": {"on"}}); r.StatusCode == http.StatusSeeOther {
		t.Error("a plain user changed the setting")
	}
	if r := e.post(t, admin, "/admin/settings/cells", url.Values{"everyone_may_enroll": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("turn on: %d", r.StatusCode)
	}
	if s := status(user); s != http.StatusOK {
		t.Errorf("a plain user on /enroll with the setting back on: %d", s)
	}
}
