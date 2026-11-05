// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

func (s *Server) statsExportRoutes() {
	if s.Stats == nil {
		return
	}
	s.mux.Handle("GET /statistics/export.csv", s.requireUser(func(w http.ResponseWriter, r *http.Request, u *auth.User) {
		s.statsExport(w, r, stats.Own(u.ID))
	}))
	s.mux.Handle("GET /admin/statistics/export.csv", s.requireAdmin(func(w http.ResponseWriter, r *http.Request, _ *auth.User) {
		s.statsExport(w, r, stats.Scope{})
	}))
	s.mux.Handle("GET /admin/statistics/usage", s.requireAdmin(s.usageReport))
	s.mux.Handle("GET /admin/statistics/usage.csv", s.requireAdmin(s.usageCSV))
}

// csvTables are the tables of the statistics pages that can be taken as CSV: what the page shows, for the range of days it was
// showing, and for the same scope (a user's own, or everything for an administrator).
var csvTables = []string{"fleet", "days", "reasons", "stations", "station_peaks", "peaks", "heat", "lines", "handsets", "quality"}

func (s *Server) statsExport(w http.ResponseWriter, r *http.Request, sc stats.Scope) {
	days := rangeDays(r)
	table := r.FormValue("table")
	ctx := r.Context()
	var head []string
	var rows [][]string
	itoa := strconv.Itoa
	f1 := func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }
	var err error
	switch table {
	case "days":
		var ds []stats.Day
		if ds, err = s.Stats.Days(ctx, sc, days); err == nil {
			head = []string{"date", "calls", "answered", "talk_seconds"}
			for _, d := range ds {
				rows = append(rows, []string{d.Day.Format("2006-01-02"), itoa(d.Calls), itoa(d.Answered), strconv.FormatInt(d.TalkSeconds, 10)})
			}
		}
	case "reasons":
		var rs []stats.Reason
		if rs, err = s.Stats.EndReasons(ctx, sc, days); err == nil {
			head = []string{"reason", "calls"}
			for _, x := range rs {
				rows = append(rows, []string{csvSafe(x.Reason), itoa(x.Count)})
			}
		}
	case "fleet":
		if sc.User != nil { // the whole controller's base stations are the administrators'
			http.NotFound(w, r)
			return
		}
		var fc []stats.FleetCell
		if fc, err = s.Stats.FleetCells(ctx, days); err == nil {
			head = []string{"base_station", "hardware", "software", "stub", "calls", "talk_seconds", "handsets_heard_7d", "behind"}
			for _, c := range fc {
				rows = append(rows, []string{csvSafe(c.Name), csvSafe(c.Hardware), csvSafe(c.Software), csvSafe(c.Stub), itoa(c.Calls), strconv.FormatInt(c.TalkSeconds, 10), itoa(c.Handsets), strconv.FormatBool(c.Behind)})
			}
		}
	case "stations":
		var cs []stats.CellUse
		if cs, err = s.Stats.PerCell(ctx, sc, days); err == nil {
			head = []string{"base_station", "calls", "talk_seconds"}
			for _, c := range cs {
				rows = append(rows, []string{csvSafe(c.Name), itoa(c.Calls), strconv.FormatInt(c.TalkSeconds, 10)})
			}
		}
	case "station_peaks":
		var cs []stats.CellPeak
		if cs, err = s.Stats.PeakPerCell(ctx, sc, days); err == nil {
			head = []string{"base_station", "most_calls_at_once", "capacity"}
			for _, c := range cs {
				rows = append(rows, []string{csvSafe(c.Name), itoa(c.Peak), itoa(c.Capacity)})
			}
		}
	case "peaks":
		var ps []stats.Peak
		if ps, err = s.Stats.Peaks(ctx, sc, days); err == nil {
			head = []string{"date", "most_calls_at_once"}
			for _, p := range ps {
				rows = append(rows, []string{p.Day.Format("2006-01-02"), itoa(p.Peak)})
			}
		}
	case "heat":
		var h stats.Heat
		if h, err = s.Stats.Heat(ctx, sc, days); err == nil {
			head = []string{"weekday", "hour", "calls"}
			names := [7]string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
			for d, row := range h.Cells {
				for hr, n := range row {
					rows = append(rows, []string{names[d], itoa(hr), itoa(n)})
				}
			}
		}
	case "lines", "handsets":
		head = []string{table[:len(table)-1], "calls", "answered", "missed", "talk_seconds", "measured_calls", "average_mos", "poor_calls"}
		var us []stats.Usage
		if table == "lines" {
			var lr []lineRow
			if lr, err = s.lineRows(r, sc, days); err == nil {
				head = append(head, "registered_percent")
				for _, l := range lr {
					avail := ""
					if l.HasAvail {
						avail = f1(l.Avail.UpPercent)
					}
					rows = append(rows, append(usageCells(l.Usage, itoa, f1), avail))
				}
			}
		} else if us, err = s.Stats.PerHandset(ctx, sc, days); err == nil {
			for _, u := range us {
				rows = append(rows, usageCells(u, itoa, f1))
			}
		}
	case "quality":
		var qs []stats.QualityDay
		if qs, err = s.Stats.Quality(ctx, sc, days); err == nil {
			head = []string{"date", "measured_calls", "average_mos", "poor_calls"}
			for _, q := range qs {
				mos := ""
				if q.Calls > 0 {
					mos = f1(q.AvgMOS)
				}
				rows = append(rows, []string{q.Day.Format("2006-01-02"), itoa(q.Calls), mos, itoa(q.Poor)})
			}
		}
	default:
		http.Error(w, "unknown table", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeCSV(w, fmt.Sprintf("opendectcell-%s-%dd-%s.csv", table, days, time.Now().Format("2006-01-02")), head, rows)
}

func usageCells(u stats.Usage, itoa func(int) string, f1 func(float64) string) []string {
	name := u.Name
	if u.ID == uuid.Nil && name == "" {
		name = "(removed)"
	}
	mos := ""
	if u.MOSCalls > 0 {
		mos = f1(u.AvgMOS)
	}
	return []string{csvSafe(name), itoa(u.Calls), itoa(u.Answered), itoa(u.Missed), strconv.FormatInt(u.TalkSeconds, 10), itoa(u.MOSCalls), mos, itoa(u.Poor)}
}

func writeCSV(w http.ResponseWriter, name string, head []string, rows [][]string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("\xef\xbb\xbf")) // so that a spreadsheet reads it as UTF-8
	cw := csv.NewWriter(w)
	_ = cw.Write(head)
	_ = cw.WriteAll(rows)
	cw.Flush()
}

// --- the monthly usage report ---

type usageData struct {
	Month    time.Time
	Prev     string
	Next     string
	HasNext  bool
	Users    []stats.UserUsage
	Orgs     []stats.OrgUsage
	MonthKey string
}

// reportMonth reads "YYYY-MM" (the local month); anything else is the month before this one, the one a report is usually wanted for.
func reportMonth(r *http.Request, now time.Time) time.Time {
	if t, err := time.ParseInLocation("2006-01", r.FormValue("month"), now.Location()); err == nil && !t.After(now) && t.Year() >= 2020 {
		return t
	}
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return first.AddDate(0, -1, 0)
}

func (s *Server) usageReport(w http.ResponseWriter, r *http.Request, u *auth.User) {
	now := time.Now()
	m := reportMonth(r, now)
	d := usageData{Month: m, MonthKey: m.Format("2006-01"), Prev: m.AddDate(0, -1, 0).Format("2006-01")}
	if next := m.AddDate(0, 1, 0); !next.After(now) {
		d.HasNext, d.Next = true, next.Format("2006-01")
	}
	var err error
	if d.Users, err = s.Stats.MonthlyUsage(r.Context(), m); err == nil {
		d.Orgs, err = s.Stats.OrgMonthlyUsage(r.Context(), m)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "admin_usage.html", page{Title: "Usage report", User: u, Data: d})
}

func (s *Server) usageCSV(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	m := reportMonth(r, time.Now())
	itoa := strconv.Itoa
	mins := func(sec int64) string { return strconv.FormatInt((sec+30)/60, 10) }
	switch r.FormValue("table") {
	case "orgs":
		os, err := s.Stats.OrgMonthlyUsage(r.Context(), m)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		var rows [][]string
		for _, o := range os {
			rows = append(rows, []string{csvSafe(o.Name), itoa(o.Members), itoa(o.Calls), itoa(o.Answered), mins(o.TalkSeconds), itoa(o.Handsets), itoa(o.Lines)})
		}
		writeCSV(w, "opendectcell-usage-organizations-"+m.Format("2006-01")+".csv", []string{"organization", "members", "calls", "answered", "talk_minutes", "handsets", "voip_lines"}, rows)
	default:
		us, err := s.Stats.MonthlyUsage(r.Context(), m)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		var rows [][]string
		for _, u := range us {
			rows = append(rows, []string{csvSafe(u.Username), csvSafe(u.DisplayName), itoa(u.Calls), itoa(u.Answered), mins(u.TalkSeconds), itoa(u.Handsets), itoa(u.Lines)})
		}
		writeCSV(w, "opendectcell-usage-users-"+m.Format("2006-01")+".csv", []string{"username", "name", "calls", "answered", "talk_minutes", "handsets", "voip_lines"}, rows)
	}
}
