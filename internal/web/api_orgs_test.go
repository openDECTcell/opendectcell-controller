// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/org"
)

// The organization and trust endpoints, every response checked against docs/api/openapi.yaml by the client.
func TestOrganizationAPI(t *testing.T) {
	e := newEnvWith(t, func(s *Server) { s.Orgs = &org.Store{Pool: s.Pool} })
	for _, name := range []string{"carol", "dave"} {
		if _, err := e.store.CreateLocalUser(ctx, auth.NewLocalUser{Username: name, Password: name + " password", Role: auth.RoleUser}); err != nil {
			t.Fatal(err)
		}
	}
	login := func(user, pw string) *apiClient {
		c := e.browser()
		e.login(t, c, user, pw)
		return newAPIClient(t, e, c)
	}
	owner, carol, dave := login("user", "user password"), login("carol", "carol password"), login("dave", "dave password")
	anon := newAPIClient(t, e, e.browser())
	anon.do("GET", "/organizations", nil, http.StatusUnauthorized)
	anon.do("GET", "/me/trust", nil, http.StatusUnauthorized)

	if m := owner.do("GET", "/me/trust", nil, http.StatusOK); m["scope"] != "all" || m["usable_cells"] != float64(0) {
		t.Errorf("default trust: %v", m)
	}
	owner.do("POST", "/organizations", map[string]any{"name": " "}, http.StatusBadRequest)
	o := owner.do("POST", "/organizations", map[string]any{"name": "Makerspace"}, http.StatusCreated)
	id := o["id"].(string)
	if o["owner"] != true || o["role"] != "admin" || o["display_name"] != "Makerspace" {
		t.Errorf("created: %v", o)
	}
	if l := owner.do("GET", "/organizations", nil, http.StatusOK); len(l["items"].([]any)) != 1 {
		t.Errorf("list: %v", l)
	}

	// A stranger learns nothing.
	dave.do("GET", "/organizations/"+id, nil, http.StatusNotFound)
	dave.do("POST", "/organizations/"+id+"/invitations", nil, http.StatusNotFound)
	dave.do("DELETE", "/organizations/"+id, nil, http.StatusNotFound)

	// Invite, preview (uses nothing), join with a name of one's own, once.
	inv := owner.do("POST", "/organizations/"+id+"/invitations", nil, http.StatusCreated)
	tok := inv["token"].(string)
	if inv["join_url"] == "" || inv["expires_at"] == nil {
		t.Errorf("invitation: %v", inv)
	}
	if m := carol.do("POST", "/organizations/invitation-preview", map[string]any{"token": tok}, http.StatusOK); m["organization_name"] != "Makerspace" {
		t.Errorf("preview: %v", m)
	}
	carol.do("POST", "/organizations/invitation-preview", map[string]any{"token": "nothing-like-a-token-0123456789"}, http.StatusBadRequest)
	j := carol.do("POST", "/organizations/join", map[string]any{"token": tok, "display_name": "Crew"}, http.StatusOK)
	if j["display_name"] != "Crew" || j["name"] != "Makerspace" || j["role"] != "member" || j["owner"] != false {
		t.Errorf("joined: %v", j)
	}
	if m := dave.do("POST", "/organizations/join", map[string]any{"token": tok}, http.StatusBadRequest); m["code"] != "bad_invitation" {
		t.Errorf("a used token: %v", m)
	}
	owner.do("POST", "/organizations/join", map[string]any{"token": inv["token"]}, http.StatusBadRequest)

	// The overview: counts, people, what the caller may do.
	ov := carol.do("GET", "/organizations/"+id, nil, http.StatusOK)
	if people := ov["people"].([]any); len(people) != 2 || ov["can_invite"] != false || ov["can_delete"] != false {
		t.Errorf("member's overview: %v", ov)
	}
	if c := ov["cells"].(map[string]any); c["total"] != float64(0) || c["offline"] != float64(0) {
		t.Errorf("cells: %v", c)
	}
	var carolID string
	for _, p := range ov["people"].([]any) {
		if m := p.(map[string]any); m["name"] == "carol" {
			carolID = m["user_id"].(string)
		}
	}

	// Who may do what: a member neither renames nor invites; the owner does, and appoints.
	carol.do("PATCH", "/organizations/"+id, map[string]any{"name": "Mine"}, http.StatusForbidden)
	carol.do("PATCH", "/organizations/"+id, map[string]any{}, http.StatusBadRequest)
	if m := carol.do("PATCH", "/organizations/"+id, map[string]any{"display_name": "Our crew"}, http.StatusOK); m["display_name"] != "Our crew" {
		t.Errorf("own name: %v", m)
	}
	if m := owner.do("PATCH", "/organizations/"+id, map[string]any{"name": "Makers"}, http.StatusOK); m["name"] != "Makers" {
		t.Errorf("rename: %v", m)
	}
	carol.do("POST", "/organizations/"+id+"/invitations", nil, http.StatusForbidden)
	carol.do("PATCH", "/organizations/"+id+"/members/"+carolID, map[string]any{"role": "admin"}, http.StatusForbidden)
	owner.do("PATCH", "/organizations/"+id+"/members/"+carolID, map[string]any{"role": "king"}, http.StatusBadRequest)
	owner.do("PATCH", "/organizations/"+id+"/members/"+carolID, map[string]any{"role": "admin"}, http.StatusOK)
	carol.do("POST", "/organizations/"+id+"/invitations", nil, http.StatusCreated)

	// Trust: an organization she belongs to, an unknown setting, a setting that strands her handset.
	carol.do("PUT", "/me/trust", map[string]any{"scope": "everybody"}, http.StatusBadRequest)
	tr := carol.do("PUT", "/me/trust", map[string]any{"scope": "orgs", "organization_ids": []string{id}}, http.StatusOK)
	if tr["scope"] != "orgs" || len(tr["organization_ids"].([]any)) != 1 {
		t.Errorf("trust: %v", tr)
	}
	var uid string
	if err := e.app.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'carol'").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Pool.Exec(ctx, `INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\x0102030405', 1, '\x00')`, uid); err != nil {
		t.Fatal(err)
	}
	if m := carol.do("PUT", "/me/trust", map[string]any{"scope": "own"}, http.StatusConflict); m["code"] != "no_usable_cell" {
		t.Errorf("the refusal: %v", m)
	}
	if m := carol.do("GET", "/me/trust", nil, http.StatusOK); m["scope"] != "orgs" {
		t.Errorf("the refused setting was saved: %v", m)
	}
	m := carol.do("PUT", "/me/trust", map[string]any{"scope": "own", "confirm": true}, http.StatusOK)
	if w := m["warnings"].([]any); len(w) != 1 || w[0] != "no_usable_cell" || m["handsets"] != float64(1) {
		t.Errorf("confirmed: %v", m)
	}

	// Removing and leaving; the owner stays; deleting.
	owner.do("DELETE", "/organizations/"+id+"/members/"+carolID, nil, http.StatusNoContent)
	carol.do("GET", "/organizations/"+id, nil, http.StatusNotFound)
	owner.do("DELETE", "/organizations/"+id+"/membership", nil, http.StatusConflict)
	owner.do("DELETE", "/organizations/"+id, nil, http.StatusNoContent)
	owner.do("GET", "/organizations/"+id, nil, http.StatusNotFound)
}
