// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/sipevents"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

func (s *Server) statsRoutes() {
	if s.Stats == nil {
		return
	}
	s.mux.Handle("GET /statistics", s.requireUser(s.statisticsUser))
	s.mux.Handle("GET /admin/statistics", s.requireAdmin(s.statisticsAdmin))
	s.mux.Handle("GET /admin", s.requireAdmin(s.adminDashboard))
	s.statsExportRoutes()
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
	// the fleet view of the administrators: the active base stations side by side, and how the software versions are spread
	FleetCells []stats.FleetCell
	Versions   []stats.VersionGroup
	Busiest    *stats.FleetCell
	Quietest   *stats.FleetCell

	// the series of C1, and the comparison of C2
	Heat      stats.Heat
	Peaks     []stats.Peak
	PeakMax   int
	CellPeaks []cellPeakRow
	Lines     []lineRow
	Handsets  []stats.Usage
	Quality   []stats.QualityDay
	Worst     []stats.WorstCall
	Cur, Prev stats.Period
	Export    string // the address the CSV links start from
}

// cellPeakRow is a base station's peak with what is made of it against its capacity.
type cellPeakRow struct {
	stats.CellPeak
	Full bool // the peak reached what it can carry
}

// lineRow is a VoIP line's use, and how much of the time it was registered.
type lineRow struct {
	stats.Usage
	Avail    sipevents.Summary
	HasAvail bool
}

// maxLineRows is how many VoIP lines the administrators' table shows (the least available first).
const maxLineRows = 30

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
	if err == nil {
		err = s.moreStats(r, sc, &d)
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
		if d.FleetCells, err = s.Stats.FleetCells(r.Context(), d.Days); err != nil {
			s.internalError(w, r, err)
			return
		}
		d.Versions = stats.VersionSpread(d.FleetCells)
		if n := len(d.FleetCells); n > 1 && d.FleetCells[0].Calls > d.FleetCells[n-1].Calls {
			d.Busiest, d.Quietest = &d.FleetCells[0], &d.FleetCells[n-1]
		}
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

// moreStats fills in the series of C1 and the comparison of C2.
func (s *Server) moreStats(r *http.Request, sc stats.Scope, d *statsData) error {
	ctx := r.Context()
	var err error
	d.Export = "/statistics/export.csv"
	if d.All {
		d.Export = "/admin/statistics/export.csv"
	}
	if d.Heat, err = s.Stats.Heat(ctx, sc, d.Days); err != nil {
		return err
	}
	if d.Peaks, err = s.Stats.Peaks(ctx, sc, d.Days); err != nil {
		return err
	}
	for _, p := range d.Peaks {
		d.PeakMax = max(d.PeakMax, p.Peak)
	}
	cps, err := s.Stats.PeakPerCell(ctx, sc, d.Days)
	if err != nil {
		return err
	}
	for _, c := range cps {
		d.CellPeaks = append(d.CellPeaks, cellPeakRow{CellPeak: c, Full: c.Capacity > 0 && c.Peak >= c.Capacity})
	}
	if d.Handsets, err = s.Stats.PerHandset(ctx, sc, d.Days); err != nil {
		return err
	}
	if d.Quality, err = s.Stats.Quality(ctx, sc, d.Days); err != nil {
		return err
	}
	if sc.User != nil { // the details of a call are its owner's
		if d.Worst, err = s.Stats.Worst(ctx, sc, d.Days, 5); err != nil {
			return err
		}
	}
	if d.Cur, d.Prev, err = s.Stats.Compare(ctx, sc, d.Days); err != nil {
		return err
	}
	d.Lines, err = s.lineRows(r, sc, d.Days)
	return err
}

// lineRows joins what each VoIP line did with how available it was (from its registration history). Lines without a call are in it.
func (s *Server) lineRows(r *http.Request, sc stats.Scope, days int) ([]lineRow, error) {
	ctx := r.Context()
	use, err := s.Stats.PerLine(ctx, sc, days)
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, "SELECT id, label FROM sip_account WHERE $1::uuid IS NULL OR owner_user_id = $1 ORDER BY lower(label), id", sc.User)
	if err != nil {
		return nil, err
	}
	byID := map[uuid.UUID]*lineRow{}
	var order []uuid.UUID
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			rows.Close()
			return nil, err
		}
		byID[id] = &lineRow{Usage: stats.Usage{ID: id, Name: label}}
		order, ids = append(order, id), append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var gone *lineRow
	for _, u := range use {
		if row, ok := byID[u.ID]; ok {
			row.Usage = u
		} else if u.ID == uuid.Nil {
			gone = &lineRow{Usage: u}
		}
	}
	now := time.Now()
	window := time.Duration(days) * 24 * time.Hour
	events, err := sipevents.WindowMany(ctx, s.Pool, ids, now, window)
	if err != nil {
		return nil, err
	}
	out := make([]lineRow, 0, len(order)+1)
	for _, id := range order {
		row := byID[id]
		row.Avail = sipevents.Summarise(events[id], now, window)
		row.HasAvail = row.Avail.Counted > 0
		out = append(out, *row)
	}
	if sc.User == nil {
		// everybody's lines: the ones that deserve a look come first, and not more than fit a page
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if a.HasAvail != b.HasAvail {
				return a.HasAvail // a line with a record before one without
			}
			return a.Avail.UpPercent < b.Avail.UpPercent
		})
		if len(out) > maxLineRows {
			out = out[:maxLineRows]
		}
	}
	if gone != nil {
		out = append(out, *gone)
	}
	return out, nil
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
