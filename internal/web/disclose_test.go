// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
)

// A user discloses a handset or a base station to the administrators one entry at a time; an
// administrator then sees it under that user in Administration > Users, and only that.
func TestDiscloseToAdministrators(t *testing.T) {
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
	var uid uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	ins := func(n int, name string) uuid.UUID {
		var id uuid.UUID
		if err := e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name, model) VALUES ($1, $2, $3, '\\x', $4, 'Gigaset A540') RETURNING id",
			uid, []byte{byte(n)}, n, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	kitchen, cellar := ins(1, "Kitchen"), ins(2, "Cellar")
	var acct, hall, barn uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Secret line', '1001', 'sip.example.org') RETURNING id", uid).Scan(&acct)
	e.store.Pool.Exec(ctx, "INSERT INTO handset_line (handset_id, line, sip_account_id) VALUES ($1, 1, $2)", kitchen, acct)
	e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state, enrolled_by) VALUES ('Hall', 'active', $1) RETURNING id", uid).Scan(&hall)
	e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state, enrolled_by) VALUES ('Barn', 'active', $1) RETURNING id", uid).Scan(&barn)

	u, a := e.userBrowser(t), e.adminBrowser(t)
	page := "/admin/users/" + uid.String()
	adminSees := func(what string) bool { _, b := e.get(t, a, page); return strings.Contains(b, what) }

	// nothing is disclosed to begin with
	if adminSees("Kitchen") || adminSees("Cellar") || adminSees("Hall") || adminSees("Barn") {
		t.Fatal("an admin sees something nobody disclosed")
	}
	// the checkbox is on each page, off
	for _, p := range []string{"/handsets/" + kitchen.String(), "/stations/" + hall.String()} {
		if _, b := e.get(t, u, p); !strings.Contains(b, "Disclose to administrators") || strings.Contains(b, `name="disclosed" checked`) {
			t.Errorf("%s: no unticked disclosure box", p)
		}
	}
	// the owner discloses one handset and one base station
	if r := e.post(t, u, "/handsets/"+kitchen.String(), url.Values{"name": {"Kitchen"}, "dual_ring": {"busy"}, "disclosed": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("save handset: %d", r.StatusCode)
	}
	if r := e.post(t, u, "/stations/"+hall.String(), url.Values{"name": {"Hall"}, "precision": {"exact"}, "disclosed": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("save station: %d", r.StatusCode)
	}
	if !adminSees("Kitchen") || !adminSees("Hall") {
		t.Error("the admin does not see what was disclosed")
	}
	if adminSees("Cellar") || adminSees("Barn") {
		t.Error("the admin sees what was not disclosed")
	}
	if adminSees("Secret line") || adminSees("1001") {
		t.Error("the admin sees the handset's VoIP line")
	}
	if _, b := e.get(t, u, "/handsets/"+kitchen.String()); !strings.Contains(b, `name="disclosed" checked`) {
		t.Error("the handset page does not show it as disclosed")
	}
	// disclosure gives the admin no way into the handset itself
	if r, _ := e.get(t, a, "/handsets/"+kitchen.String()); r.StatusCode != http.StatusNotFound {
		t.Errorf("an admin on a disclosed handset's page: %d", r.StatusCode)
	}
	// only the operator decides: an admin saving the station leaves it, and cannot switch Barn on
	if r := e.post(t, a, "/stations/"+hall.String(), url.Values{"name": {"Hall"}, "precision": {"exact"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin save of the station: %d", r.StatusCode)
	}
	if r := e.post(t, a, "/stations/"+barn.String(), url.Values{"name": {"Barn"}, "precision": {"exact"}, "disclosed": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin save of the other station: %d", r.StatusCode)
	}
	if !adminSees("Hall") || adminSees("Barn") {
		t.Error("an admin's save changed what the operator disclosed")
	}
	// a user's page lists nothing for another user, and a plain user cannot read the admin page
	if r, _ := e.get(t, u, page); r.StatusCode == http.StatusOK {
		t.Error("a user reached Administration > Users")
	}
	// the owner takes it back
	if r := e.post(t, u, "/handsets/"+kitchen.String(), url.Values{"name": {"Kitchen"}, "dual_ring": {"busy"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("unticking the handset: %d", r.StatusCode)
	}
	if r := e.post(t, u, "/stations/"+hall.String(), url.Values{"name": {"Hall"}, "precision": {"exact"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("unticking the station: %d", r.StatusCode)
	}
	if adminSees("Kitchen") || adminSees("Hall") {
		t.Error("the admin still sees what was taken back")
	}
	// the API names the operator of a base station only if it was disclosed
	c := &cell.Cell{ID: hall, Name: "Hall", EnrolledBy: &uid}
	if got := e.app.cellJSON(c, true)["enrolled_by"]; got != nil {
		t.Errorf("the API names the operator of an undisclosed base station: %v", got)
	}
	c.Disclosed = true
	if got, _ := e.app.cellJSON(c, true)["enrolled_by"].(*uuid.UUID); got == nil || *got != uid {
		t.Error("the API leaves out the operator of a disclosed base station")
	}
	_ = cellar
}

// An administrator who does not operate a base station sees its position only if it was disclosed, and cannot change
// the operator's position or sharing switches (review finding W5).
func TestAdminSeesOnlyDisclosedStationDetails(t *testing.T) {
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
	var uid, hall uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, `INSERT INTO cell (name, state, enrolled_by, latitude, longitude, position_precision)
		VALUES ('Hall', 'active', $1, 52.123456, 9.654321, 'exact') RETURNING id`, uid).Scan(&hall)
	a := e.adminBrowser(t)
	page := "/stations/" + hall.String()

	if _, b := e.get(t, a, page); strings.Contains(b, "52.123456") || strings.Contains(b, "9.654321") {
		t.Error("an admin sees the position of a base station that was not disclosed")
	}
	// the admin saves the page (the name, say): the operator's position and switches stay
	e.store.Pool.Exec(ctx, "UPDATE cell SET share_position = false, share_name = false, share_stats = false WHERE id = $1", hall)
	if r := e.post(t, a, page, url.Values{"name": {"Hall B"}, "precision": {"region"}, "share_position": {"on"}, "share_name": {"on"}, "share_stats": {"on"}, "lat": {"1"}, "lon": {"2"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin save: %d", r.StatusCode)
	}
	var name, prec string
	var lat float64
	var sp, sn, ss bool
	e.store.Pool.QueryRow(ctx, "SELECT name, position_precision, latitude, share_position, share_name, share_stats FROM cell WHERE id = $1", hall).Scan(&name, &prec, &lat, &sp, &sn, &ss)
	if name != "Hall B" || prec != "exact" || lat != 52.123456 || sp || sn || ss {
		t.Errorf("an admin's save changed the operator's choices: %q %q %v %v %v %v", name, prec, lat, sp, sn, ss)
	}
	// disclosed, the admin sees it
	e.store.Pool.Exec(ctx, "UPDATE cell SET disclosed_to_admins = true WHERE id = $1", hall)
	if _, b := e.get(t, a, page); !strings.Contains(b, "52.123456") {
		t.Error("an admin does not see the position of a disclosed base station")
	}
}
