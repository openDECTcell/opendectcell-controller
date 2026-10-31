// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

// metrics serves Prometheus text for a monitoring system. It exists only when
// a token is configured (OPENDECTCELL_HTTP_METRICS_TOKEN) and needs it as a bearer
// token; it names cells, so it is not for the public.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.MetricsToken == "" {
		http.NotFound(w, r)
		return
	}
	got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	want := sha256.Sum256([]byte(s.MetricsToken))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var b strings.Builder
	line := func(help, typ, name string, v any, labels ...string) {
		if help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		}
		if len(labels) > 0 {
			fmt.Fprintf(&b, "%s{%s} %v\n", name, strings.Join(labels, ","), v)
		} else {
			fmt.Fprintf(&b, "%s %v\n", name, v)
		}
	}
	lbl := func(k, v string) string {
		v = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
		return k + `="` + v + `"`
	}
	line("Build information.", "gauge", "opendectcell_build_info", 1, lbl("version", s.Version))
	if !s.StartedAt.IsZero() {
		line("Seconds since the controller started.", "gauge", "opendectcell_uptime_seconds", int64(time.Since(s.StartedAt).Seconds()))
	}
	if s.Stats != nil {
		if f, err := s.Stats.Fleet(r.Context()); err == nil {
			line("Cells by state.", "gauge", "opendectcell_cells", f.CellsActive, lbl("state", "active"))
			line("", "gauge", "opendectcell_cells", f.CellsPending, lbl("state", "pending"))
			line("", "gauge", "opendectcell_cells", f.CellsDisabled, lbl("state", "disabled"))
			line("Users that can log in.", "gauge", "opendectcell_users", f.Users)
			line("Users waiting for approval.", "gauge", "opendectcell_users_pending", f.UsersPending)
			line("Active cells whose certificate ends within 14 days without a successor.", "gauge", "opendectcell_cells_certificate_expiring", f.CertsExpiring)
		}
		if m, err := s.Stats.Summary(r.Context(), stats.Scope{}); err == nil {
			line("Handsets registered.", "gauge", "opendectcell_handsets", m.Handsets)
			line("Calls recorded, all time.", "counter", "opendectcell_calls_total", m.CallsTotal)
			line("Calls started in the last 24 hours.", "gauge", "opendectcell_calls_24h", m.Calls24h)
		}
	}
	if s.Cells != nil {
		cells, err := s.Cells.ListCells(r.Context(), &auth.User{Role: auth.RoleAdmin})
		if err == nil {
			sort.Slice(cells, func(i, j int) bool { return cells[i].Name < cells[j].Name })
			first, online := true, 0
			for _, c := range cells {
				if c.State != "active" {
					continue
				}
				up := 0
				if s.liveStatus(c.ID).Online {
					up = 1
					online++
				}
				h := ""
				if first {
					h = "Whether an active cell's link is up."
					first = false
				}
				line(h, "gauge", "opendectcell_cell_up", up, lbl("cell", c.Name))
			}
			line("Active cells whose link is up.", "gauge", "opendectcell_cells_online", online)
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}
