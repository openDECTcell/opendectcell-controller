// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"strconv"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

func (s *Server) statsRoutes() {
	if s.Stats == nil {
		return
	}
	s.mux.Handle("GET /statistics", s.requireUser(s.statisticsUser))
	s.mux.Handle("GET /admin/statistics", s.requireAdmin(s.statisticsAdmin))
	s.mux.Handle("GET /admin", s.requireAdmin(s.adminDashboard))
}

type statsData struct {
	All     bool // the whole controller
	Days    int
	Summary stats.Summary
	Series  []stats.Day
	Reasons []stats.Reason
	Cells   []stats.CellUse
	Fleet   *stats.Fleet
	Live    *fleetLive
}

// fleetLive counts the cells' links right now.
type fleetLive struct {
	Online, Offline int
	Rows            []cellRow
}

func rangeDays(r *http.Request) int {
	switch n, _ := strconv.Atoi(r.FormValue("days")); n {
	case 7, 14, 90:
		return n
	}
	return 30
}

func (s *Server) statsPage(w http.ResponseWriter, r *http.Request, u *auth.User, sc stats.Scope, title string) {
	d := statsData{All: sc.User == nil, Days: rangeDays(r)}
	var err error
	if d.Summary, err = s.Stats.Summary(r.Context(), sc); err == nil {
		d.Series, err = s.Stats.Days(r.Context(), sc, d.Days)
	}
	if err == nil {
		d.Reasons, err = s.Stats.EndReasons(r.Context(), sc, d.Days)
	}
	if err == nil {
		d.Cells, err = s.Stats.PerCell(r.Context(), sc, d.Days)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if d.All {
		f, err := s.Stats.Fleet(r.Context())
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		d.Fleet = &f
		if s.Cells != nil {
			d.Live, err = s.liveFleet(r, u)
			if err != nil {
				s.internalError(w, r, err)
				return
			}
		}
	}
	s.render(w, http.StatusOK, "statistics.html", page{Title: title, User: u, Data: d})
}

func (s *Server) liveFleet(r *http.Request, u *auth.User) (*fleetLive, error) {
	cells, err := s.Cells.ListCells(r.Context(), u)
	if err != nil {
		return nil, err
	}
	l := &fleetLive{}
	for _, c := range cells {
		if c.State != "active" {
			continue
		}
		row := cellRow{c, s.liveStatus(c.ID)}
		if row.Live.Online {
			l.Online++
		} else {
			l.Offline++
		}
		l.Rows = append(l.Rows, row)
	}
	return l, nil
}

func (s *Server) statisticsUser(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.statsPage(w, r, u, stats.Own(u.ID), "Statistics")
}

func (s *Server) statisticsAdmin(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.statsPage(w, r, u, stats.Scope{}, "Statistics, whole controller")
}
