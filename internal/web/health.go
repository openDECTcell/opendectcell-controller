// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/backup"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
)

// Health statuses.
const (
	healthOK   = "ok"
	healthWarn = "warn"
	healthFail = "fail"
	healthInfo = "info" // nothing to check, or not set up on purpose
)

type healthCheck struct {
	Name   string
	Status string
	Detail string
	Href   string // where to look at it or fix it
}

func (s *Server) healthRoutes() {
	s.mux.Handle("GET /admin/health", s.requireAdmin(s.healthPage))
	s.mux.Handle("POST /admin/health/mail", s.requireAdmin(s.healthMail))
	s.mux.HandleFunc("GET /status", s.publicStatus)
}

// healthChecks runs every check, each with a time limit of its own: a slow one is a finding, not a hung page.
func (s *Server) healthChecks(ctx context.Context) []healthCheck {
	fns := []func(context.Context) healthCheck{s.checkDatabase, s.checkSchema, s.checkClock, s.checkMail, s.checkMailQueue, s.checkStations, s.checkCertificates,
		s.checkLines, s.checkBackup, s.checkBackupSpace}
	out := make([]healthCheck, len(fns))
	var wg sync.WaitGroup
	for i, f := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			out[i] = f(cctx)
		}()
	}
	wg.Wait()
	return out
}

// healthSummary is the worst status among the checks.
func healthSummary(cs []healthCheck) string {
	worst := healthOK
	for _, c := range cs {
		switch c.Status {
		case healthFail:
			return healthFail
		case healthWarn:
			worst = healthWarn
		}
	}
	return worst
}

func (s *Server) checkDatabase(ctx context.Context) healthCheck {
	c := healthCheck{Name: "Database"}
	t0 := time.Now()
	if err := s.Pool.Ping(ctx); err != nil {
		c.Status, c.Detail = healthFail, "The database does not answer: "+err.Error()
		return c
	}
	c.Status, c.Detail = databaseHealth(time.Since(t0))
	return c
}

// databaseHealth judges how fast the database answered: over half a second is slow.
func databaseHealth(d time.Duration) (status, detail string) {
	if d > 500*time.Millisecond {
		return healthWarn, fmt.Sprintf("Answers, but slowly: %d ms.", d.Milliseconds())
	}
	return healthOK, fmt.Sprintf("Answers in %d ms.", d.Milliseconds())
}

func (s *Server) checkSchema(ctx context.Context) healthCheck {
	c := healthCheck{Name: "Database schema"}
	m, err := db.NewMigrator(s.Pool)
	if err != nil {
		c.Status, c.Detail = healthFail, "The migrations of this program cannot be read."
		return c
	}
	cur, latest, err := m.Status(ctx)
	switch {
	case err != nil:
		c.Status, c.Detail = healthFail, "The schema is not in step with this program: "+err.Error()
	case cur < latest:
		c.Status, c.Detail = healthWarn, fmt.Sprintf("%d of %d migrations are applied: run `migrate up` (this program expects the rest).", cur, latest)
	case cur > latest:
		c.Status, c.Detail = healthWarn, "The database is newer than this program."
	default:
		c.Status, c.Detail = healthOK, fmt.Sprintf("All %d migrations are applied.", latest)
	}
	return c
}

func (s *Server) checkClock(ctx context.Context) healthCheck {
	c := healthCheck{Name: "Clock"}
	var dbNow time.Time
	t0 := time.Now()
	if err := s.Pool.QueryRow(ctx, "SELECT now()").Scan(&dbNow); err != nil {
		c.Status, c.Detail = healthInfo, "Could not be compared with the database's clock."
		return c
	}
	c.Status, c.Detail = clockHealth(dbNow.Sub(t0.Add(time.Since(t0)/2)), time.Now().Location().String())
	return c
}

// clockHealth judges the difference between this clock and the database's: over 2 seconds is worth a look, over 30 is a failure
// (certificates, sessions and calls depend on the time).
func clockHealth(skew time.Duration, zone string) (status, detail string) {
	if skew < 0 {
		skew = -skew
	}
	switch {
	case skew > 30*time.Second:
		return healthFail, fmt.Sprintf("This clock and the database's differ by %s: certificates, sessions and calls depend on the time. Check the time service of the host.", skew.Truncate(time.Second))
	case skew > 2*time.Second:
		return healthWarn, fmt.Sprintf("This clock and the database's differ by %s.", skew.Truncate(100*time.Millisecond))
	}
	return healthOK, "Agrees with the database's clock (zone " + zone + ")."
}

func (s *Server) checkMail(context.Context) healthCheck {
	c := healthCheck{Name: "Outgoing mail", Href: "/admin/health"}
	if s.mailOn() {
		c.Status, c.Detail = healthOK, "A mail server is set up ("+s.SMTPHost+"). Send a test mail below to see that it works."
		return c
	}
	c.Status, c.Detail = healthWarn, "No mail server is set up: password resets, sign-up mails, alerts and missed-call mails cannot be sent."
	return c
}

func (s *Server) checkMailQueue(ctx context.Context) healthCheck {
	c := healthCheck{Name: "Notification mail"}
	var n int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM notification WHERE mail_wanted AND mailed_at IS NULL AND at < now() - interval '1 hour'").Scan(&n); err != nil {
		c.Status, c.Detail = healthInfo, "Could not be read."
		return c
	}
	switch {
	case n == 0:
		c.Status, c.Detail = healthOK, "Nothing waits for mail longer than an hour (apart from quiet hours)."
	case !s.mailOn():
		c.Status, c.Detail = healthWarn, fmt.Sprintf("%d notifications wait for mail and no mail server is set up.", n)
	default:
		c.Status, c.Detail = healthWarn, fmt.Sprintf("%d notifications have waited for mail for over an hour: they are held back by quiet hours or the mail server is not taking them.", n)
	}
	return c
}

func (s *Server) checkStations(ctx context.Context) healthCheck {
	c := healthCheck{Name: "Base stations", Href: "/cells"}
	if s.Cells == nil {
		c.Status, c.Detail = healthInfo, "This controller has no base stations (the cell side is off)."
		return c
	}
	cells, err := s.Cells.ListCells(ctx, &auth.User{Role: auth.RoleAdmin})
	if err != nil {
		c.Status, c.Detail = healthInfo, "Could not be read."
		return c
	}
	active, online := 0, 0
	for _, ce := range cells {
		if ce.State != "active" {
			continue
		}
		active++
		if s.liveStatus(ce.ID).Online {
			online++
		}
	}
	switch {
	case active == 0:
		c.Status, c.Detail = healthInfo, "No base station is enrolled and active yet."
	case online == active:
		c.Status, c.Detail = healthOK, fmt.Sprintf("All %d active base stations are connected.", active)
	case online == 0:
		c.Status, c.Detail = healthFail, fmt.Sprintf("None of the %d active base stations is connected: no handset can be reached.", active)
	default:
		c.Status, c.Detail = healthWarn, fmt.Sprintf("%d of %d active base stations are connected.", online, active)
	}
	return c
}

func (s *Server) checkCertificates(ctx context.Context) healthCheck {
	c := healthCheck{Name: "Certificates", Href: "/admin/statistics"}
	var end *time.Time
	if err := s.Pool.QueryRow(ctx, "SELECT max(not_after) FROM cell_ca WHERE active").Scan(&end); err != nil || end == nil {
		c.Status, c.Detail = healthInfo, "There is no base station certificate authority yet."
		return c
	}
	days := int(time.Until(*end).Hours() / 24)
	expiring := 0
	if s.Stats != nil {
		if f, err := s.Stats.Fleet(ctx); err == nil {
			expiring = f.CertsExpiring
		}
	}
	switch {
	case days < 0:
		c.Status, c.Detail = healthFail, "The certificate authority of the base stations has ended: no base station can connect. See the runbook in the wiki (Operating)."
	case days < 14 || expiring > 0:
		c.Status, c.Detail = healthFail, fmt.Sprintf("The authority ends in %d days; %d base station certificates end within 14 days and could not renew.", days, expiring)
	case days < certWarnDays:
		c.Status, c.Detail = healthWarn, fmt.Sprintf("The certificate authority of the base stations ends in %d days (%s). See the runbook in the wiki (Operating).", days, end.Local().Format("2006-01-02"))
	default:
		c.Status, c.Detail = healthOK, fmt.Sprintf("The base station authority is good for %d more days. The web address's certificate is the reverse proxy's: it is not checked here.", days)
	}
	return c
}

func (s *Server) checkLines(ctx context.Context) healthCheck {
	c := healthCheck{Name: "VoIP lines", Href: "/sip-accounts"}
	if s.SIP == nil {
		c.Status, c.Detail = healthInfo, "The SIP side is off."
		return c
	}
	rows, err := s.Pool.Query(ctx, "SELECT id FROM sip_account WHERE enabled AND register")
	if err != nil {
		c.Status, c.Detail = healthInfo, "Could not be read."
		return c
	}
	var total, up, down int
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) != nil {
			continue
		}
		total++
		st, ok := s.SIP.Status(id)
		switch {
		case ok && st.State == sip.RegRegistered:
			up++
		case ok && (st.State == sip.RegFailed || st.State == sip.RegBackoff):
			down++
		}
	}
	rows.Close()
	switch {
	case total == 0:
		c.Status, c.Detail = healthInfo, "No VoIP line is switched on to register."
	case down == 0:
		c.Status, c.Detail = healthOK, fmt.Sprintf("%d of %d lines are registered.", up, total)
	default:
		c.Status, c.Detail = healthWarn, fmt.Sprintf("%d of %d lines cannot register.", down, total)
	}
	return c
}

func (s *Server) checkBackup(context.Context) healthCheck {
	c := healthCheck{Name: "Backups", Href: "/admin/backup"}
	if s.BackupDir == "" {
		c.Status, c.Detail = healthInfo, "No backup directory is set."
		return c
	}
	st, err := backup.Read(s.BackupDir)
	switch {
	case errors.Is(err, backup.ErrNone):
		c.Status, c.Detail = healthWarn, "The backup service has not written a state: it is not running, or this controller cannot see its volume. Make backups by hand, or start the service."
	case err != nil:
		c.Status, c.Detail = healthWarn, "The state of the backup service cannot be read: "+err.Error()
	case st.Problem(time.Now()) != "":
		c.Status, c.Detail = healthFail, st.Problem(time.Now())+"."
	case !st.LastGood.IsZero():
		c.Status, c.Detail = healthOK, "The last good backup is from "+st.LastGood.Local().Format("2006-01-02 15:04")+"."
	default:
		c.Status, c.Detail = healthInfo, "The backup service is running and has not made a backup yet."
	}
	return c
}

func (s *Server) checkBackupSpace(context.Context) healthCheck {
	c := healthCheck{Name: "Space for backups", Href: "/admin/backup"}
	if s.BackupDir == "" {
		c.Status, c.Detail = healthInfo, "No backup directory is set."
		return c
	}
	free := s.freeSpace
	if free == nil {
		free = backup.FreeSpace
	}
	f, total, err := free(s.BackupDir)
	if err != nil || total == 0 {
		c.Status, c.Detail = healthInfo, "The free space of the backup volume is not known."
		return c
	}
	pct := float64(f) / float64(total) * 100
	text := fmt.Sprintf("%.1f GB free of %.1f GB (%.0f %%).", float64(f)/1e9, float64(total)/1e9, pct)
	switch {
	case pct < 5:
		c.Status, c.Detail = healthFail, text+" Backups will fail when it is full."
	case pct < 15:
		c.Status, c.Detail = healthWarn, text
	default:
		c.Status, c.Detail = healthOK, text
	}
	return c
}

type healthView struct {
	Checks  []healthCheck
	Summary string
	Fail    int
	Warn    int
	MailOK  bool
	Email   string
	At      time.Time
}

func (s *Server) healthPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	cs := s.healthChecks(r.Context())
	v := healthView{Checks: cs, Summary: healthSummary(cs), MailOK: s.mailOn(), Email: u.Email, At: time.Now()}
	for _, c := range cs {
		switch c.Status {
		case healthFail:
			v.Fail++
		case healthWarn:
			v.Warn++
		}
	}
	s.render(w, http.StatusOK, "admin_health.html", page{Title: "Health", User: u, Data: v, Notice: r.FormValue("done"), Error: r.FormValue("error")})
}

// healthMail sends a test mail to the administrator, to see that the mail server works.
func (s *Server) healthMail(w http.ResponseWriter, r *http.Request, u *auth.User) {
	switch {
	case !s.mailOn():
		http.Redirect(w, r, "/admin/health?error=No+mail+server+is+set+up.", http.StatusSeeOther)
	case u.Email == "":
		http.Redirect(w, r, "/admin/health?error=Your+account+has+no+e-mail+address.", http.StatusSeeOther)
	default:
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if err := s.Mailer.Send(ctx, []string{u.Email}, "openDECTcell test mail", "This is a test of the mail server of your openDECTcell controller. If you read it, outgoing mail works.\n"); err != nil {
			s.Log.Warn("test mail", "err", err)
			http.Redirect(w, r, "/admin/health?error=The+mail+server+refused+it%3A+see+the+controller%27s+log.", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/admin/health?done=A+test+mail+was+sent+to+"+u.Email+".", http.StatusSeeOther) //nolint:gosec // G710: the user's own address, escaped by the redirect
	}
}

// publicStatus is the page of a hosted instance: one word and the time, for anybody, and nothing of what is inside. It exists only
// where the operator switched it on (OPENDECTCELL_PUBLIC_STATUS): ok, degraded (a check fails) or down (the database does not answer).
func (s *Server) publicStatus(w http.ResponseWriter, r *http.Request) {
	if !s.PublicStatus {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	status := "ok"
	if err := s.Pool.Ping(ctx); err != nil {
		status = "down"
	} else if healthSummary(s.healthChecks(ctx)) == healthFail {
		status = "degraded"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if status == "down" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "checked_at": time.Now().UTC().Format(time.RFC3339)})
}
