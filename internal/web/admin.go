// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"runtime"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

func (s *Server) adminRoutes() {
	s.mux.Handle("GET /admin/system", s.requireAdmin(s.adminSystem))
	s.mux.Handle("GET /admin/backup", s.requireAdmin(func(w http.ResponseWriter, r *http.Request, u *auth.User) {
		s.render(w, http.StatusOK, "admin_backup.html", page{Title: "Backup", User: u})
	}))
}

type dashData struct {
	Summary stats.Summary
	Fleet   stats.Fleet
	Live    *fleetLive
	Audit   []audit.Row
	Alerts  []string
}

// adminDashboard is the first page an admin sees: what needs attention, then
// the numbers.
func (s *Server) adminDashboard(w http.ResponseWriter, r *http.Request, u *auth.User) {
	sum, err := s.Stats.Summary(r.Context(), stats.Scope{})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	f, err := s.Stats.Fleet(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	d := dashData{Summary: sum, Fleet: f}
	if s.Cells != nil {
		if d.Live, err = s.liveFleet(r, u); err != nil {
			s.internalError(w, r, err)
			return
		}
		if d.Live.Offline > 0 {
			d.Alerts = append(d.Alerts, "Cells offline: see the list below.")
		}
	}
	if u.AuthSource == "local" && s.Auth.Keyring != nil {
		if on, err := s.Auth.HasTOTP(r.Context(), u.ID); err == nil && !on {
			d.Alerts = append(d.Alerts, "Your own account has no two-factor login. Turn it on under Account.")
		}
	}
	if f.CellsPending > 0 {
		d.Alerts = append(d.Alerts, "A cell is waiting for your approval.")
	}
	if f.UsersPending > 0 {
		d.Alerts = append(d.Alerts, "A sign-up is waiting for your approval.")
	}
	if f.CertsExpiring > 0 {
		d.Alerts = append(d.Alerts, "A cell's certificate ends within 14 days and has not been renewed.")
	}
	if d.Audit, err = audit.List(r.Context(), s.Pool, audit.Filter{Limit: 8}); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "admin_dashboard.html", page{Title: "Dashboard", User: u, Data: d})
}

type systemData struct {
	Version   string
	Go        string
	Up        string
	DB        string
	DBOK      bool
	Mail      bool
	MailHost  string
	Cells     bool
	SIP       bool
	Local     bool
	OIDC      bool
	BaseURL   string
	Now       time.Time
	Hostnames []string
}

func (s *Server) adminSystem(w http.ResponseWriter, r *http.Request, u *auth.User) {
	d := systemData{Version: s.Version, Go: runtime.Version(), Now: time.Now(), Local: s.LocalLogin, OIDC: s.OIDC != nil,
		Mail: s.mailOn(), MailHost: s.SMTPHost, Cells: s.Cells != nil, SIP: s.SIPAccounts != nil}
	if !s.StartedAt.IsZero() {
		d.Up = time.Since(s.StartedAt).Truncate(time.Second).String()
	}
	if s.BaseURL != nil {
		d.BaseURL = s.BaseURL.String()
	}
	if err := s.Pool.Ping(r.Context()); err != nil {
		d.DB = "unreachable"
	} else {
		d.DB, d.DBOK = "reachable", true
	}
	s.render(w, http.StatusOK, "admin_system.html", page{Title: "System", User: u, Data: d})
}
