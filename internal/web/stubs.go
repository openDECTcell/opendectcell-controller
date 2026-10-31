// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

// stub is a menu entry whose page is not built yet. It says what is planned,
// so the structure of the interface is visible and reviewable now.
type stub struct {
	Path, Title, Lead string
	Admin             bool
	Plans             []string
	Roadmap           string
}

var stubs = []stub{
	{Path: "/handsets", Title: "Handsets",
		Lead:    "Your handsets: register one, name it, give it up to two phone lines and a default line.",
		Plans:   []string{"List with model, lines and the base station last seen on", "Register a handset: open a PIN window on your base stations (the controller can already do this through the API)", "Name, lines, ring policy and the base stations a handset should work on", "Remove a handset"},
		Roadmap: "C6"},
	{Path: "/calls", Title: "Calls",
		Lead:    "Your call history with duration, line and quality.",
		Plans:   []string{"Your calls only, with the line and the handset used", "Filters by handset, direction and day; export as CSV", "Per call: codec, encryption and media quality, so a bad call can be explained"},
		Roadmap: "C6"},
	{Path: "/map", Title: "Public map",
		Lead:    "Base stations whose owners chose to share them, on a map.",
		Plans:   []string{"Only stations with the position switch on, at the precision their owner chose", "Name, model and statistics only where those switches are on", "Tiles from a source the admin chose; none by default, so a visit sends nothing to a third party", "A read-only JSON feed of the same data"},
		Roadmap: "C7"},
	{Path: "/admin/handsets", Title: "All handsets", Admin: true,
		Lead:    "Every handset on this controller, with owner, cell and state.",
		Plans:   []string{"Search by name, owner, model or base station", "Which cell each handset was last on and how long ago", "Move a handset to another owner, remove one, see its slot placement on each cell"},
		Roadmap: "C7"},
}

func (s *Server) stubRoutes() {
	for _, st := range stubs {
		h := func(w http.ResponseWriter, r *http.Request, u *auth.User) {
			s.render(w, http.StatusOK, "stub.html", page{Title: st.Title, User: u, Data: st})
		}
		if st.Admin {
			s.mux.Handle("GET "+st.Path, s.requireAdmin(h))
		} else {
			s.mux.Handle("GET "+st.Path, s.requireUser(h))
		}
	}
}
