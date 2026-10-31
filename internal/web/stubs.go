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
	{Path: "/calls", Title: "Calls",
		Lead:    "Your call history with duration, line and quality.",
		Plans:   []string{"Your calls only, with the line and the handset used", "Filters by handset, direction and day; export as CSV", "Per call: codec, encryption and media quality, so a bad call can be explained"},
		Roadmap: "C6"},
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
