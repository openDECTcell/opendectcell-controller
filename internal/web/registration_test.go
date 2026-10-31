// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"log/slog"
	"regexp"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/regwin"
)

func TestRegistrationWindowAPI(t *testing.T) {
	reg := regwin.New(slog.New(slog.DiscardHandler), nil, nil)
	e := newEnvWith(t, func(s *Server) { s.Registrations = reg })
	user, admin := e.browser(), e.browser()
	e.login(t, user, "user", "user password")
	e.login(t, admin, "admin", "admin password")
	ua, aa := newAPIClient(t, e, user), newAPIClient(t, e, admin)

	w := ua.do("POST", "/registration-windows", map[string]any{}, 201)
	if pin, _ := w["pin"].(string); !regexp.MustCompile(`^[0-9]{8}$`).MatchString(pin) {
		t.Errorf("pin %q", pin)
	}
	if w["state"] != "open" || w["cell_id"] != nil {
		t.Errorf("window %v", w)
	}
	id := w["id"].(string)
	ua.do("POST", "/registration-windows", map[string]any{"digits": 6}, 409)
	if g := ua.do("GET", "/registration-windows/"+id, nil, 200); g["pin"] != nil || g["state"] != "open" {
		t.Errorf("get %v: the PIN is shown once", g)
	}
	aa.do("GET", "/registration-windows/"+id, nil, 200)

	me := ua.do("GET", "/me", nil, 200)
	ua.do("POST", "/registration-windows", map[string]any{"owner_id": me["id"]}, 409)
	adm := aa.do("GET", "/me", nil, 200)
	ua.do("POST", "/registration-windows", map[string]any{"owner_id": adm["id"]}, 403)
	ua.do("POST", "/registration-windows", map[string]any{"digits": 4}, 400)

	// an admin opens a window for the user; the user may see it
	aw := aa.do("POST", "/registration-windows", map[string]any{"owner_id": me["id"], "digits": 6}, 201)
	if pin, _ := aw["pin"].(string); len(pin) != 6 || aw["owner_id"] != me["id"] {
		t.Errorf("admin's window %v", aw)
	}
	ua.do("GET", "/registration-windows/"+aw["id"].(string), nil, 200)

	ua.do("DELETE", "/registration-windows/"+id, nil, 204)
	if g := ua.do("GET", "/registration-windows/"+id, nil, 200); g["state"] != "cancelled" {
		t.Errorf("after cancel %v", g)
	}
	ua.do("POST", "/registration-windows", nil, 201)
}
