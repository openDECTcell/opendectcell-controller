// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
)

func (s *Server) publicMapRoutes() {
	if s.Cells == nil || s.Settings == nil {
		return
	}
	s.mux.HandleFunc("GET /map", s.mapPage)
	s.mux.HandleFunc("GET "+apiPrefix+"/public/stations", s.publicStations)
}

// mapPublic reports whether the map is open to visitors who are not logged in.
func (s *Server) mapPublic(r *http.Request) bool {
	m, err := s.Settings.Map(r.Context())
	return err == nil && m.Public
}

// mapPage shows the map of shared stations: to everyone when an admin made it
// public, otherwise to logged-in users.
func (s *Server) mapPage(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if u == nil && !s.mapPublic(r) {
		http.Redirect(w, r, "/login?next=%2Fmap", http.StatusSeeOther)
		return
	}
	if u != nil {
		w.Header().Set("Cache-Control", "no-store")
	}
	s.render(w, http.StatusOK, "map.html", page{Title: "Public map", User: u, Leaflet: true, Wide: u == nil})
}

// publicStations is the feed behind the map: only what each owner shared, at
// the precision they chose, and nothing for a station that shares no position.
func (s *Server) publicStations(w http.ResponseWriter, r *http.Request) {
	if u := auth.UserFrom(r.Context()); u == nil && !s.mapPublic(r) {
		http.NotFound(w, r)
		return
	}
	cells, err := s.Cells.SharedPositions(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]cell.PublicStation, 0, len(cells))
	for _, c := range cells {
		var st cell.PublicStats
		if c.ShareStats {
			st = s.publicStats(r, c)
		}
		out = append(out, c.Public(st))
	}
	w.Header().Set("Content-Type", "application/json")
	if s.mapPublic(r) {
		w.Header().Set("Cache-Control", "public, max-age=60")
	} else {
		w.Header().Set("Cache-Control", "private, no-store") // for the people who are logged in; a shared cache must not hold it
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(map[string]any{"stations": out})
}

func (s *Server) publicStats(r *http.Request, c *cell.Cell) cell.PublicStats {
	var st cell.PublicStats
	if s.Stats != nil {
		if x, err := s.Stats.Station(r.Context(), c.ID); err == nil {
			st.ConnectedHandsets, st.Calls7d, st.TalkSeconds7d = x.HandsetsSeen24h, x.Calls7d, x.TalkSeconds7d
		}
	}
	if l := s.liveStatus(c.ID); l.Online {
		st.UptimeSeconds = int64(time.Since(l.Since).Seconds())
	}
	return st
}
