// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
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
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
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
		ks, _ := secret.GenerateMasterKey()
		mk, _ := secret.ParseMasterKey(ks)
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
	if r := e.post(t, admin, "/cells/"+id+"/revoke", url.Values{"confirm": {"wrong"}}); r.StatusCode != http.StatusBadRequest || len(link.kicked) != 0 {
		t.Fatalf("revoke without confirmation: %d", r.StatusCode)
	}
	if r := e.post(t, admin, "/cells/"+id+"/revoke", url.Values{"confirm": {"Hall"}}); r.StatusCode != http.StatusSeeOther || len(link.kicked) != 1 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}

	// A plain user sees neither page; with may_add_cells, only /enroll.
	user := e.browser()
	e.login(t, user, "user", "user password")
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
