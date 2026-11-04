// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"sort"
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
	Hardware        []hardwareRow
}

// hardwareRow is how many active base stations are of one hardware model, and how many of them are connected.
type hardwareRow struct {
	Model         string
	Total, Online int
}

// unknownModel is the row for base stations that have not told their model and whose owner did not enter one.
const unknownModel = "Not reported"

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
	byModel := map[string]*hardwareRow{}
	for _, c := range cells {
		if c.State != "active" {
			continue
		}
		row := cellRow{c, s.liveStatus(c.ID)}
		model := c.Model()
		if model == "" {
			model = unknownModel
		}
		h := byModel[model]
		if h == nil {
			h = &hardwareRow{Model: model}
			byModel[model] = h
		}
		h.Total++
		if row.Live.Online {
			l.Online++
			h.Online++
		} else {
			l.Offline++
		}
		l.Rows = append(l.Rows, row)
	}
	for _, h := range byModel {
		l.Hardware = append(l.Hardware, *h)
	}
	// the most common first, the ones that did not say last, then by name
	sort.Slice(l.Hardware, func(i, j int) bool {
		a, b := l.Hardware[i], l.Hardware[j]
		if (a.Model == unknownModel) != (b.Model == unknownModel) {
			return b.Model == unknownModel
		}
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.Model < b.Model
	})
	return l, nil
}

func (s *Server) statisticsUser(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.statsPage(w, r, u, stats.Own(u.ID), "Statistics")
}

func (s *Server) statisticsAdmin(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.statsPage(w, r, u, stats.Scope{}, "Statistics, whole controller")
}
