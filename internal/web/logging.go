// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/logctl"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
)

func (s *Server) loggingRoutes() {
	if s.LogCtl == nil {
		return
	}
	s.mux.Handle("GET /logging", s.requireUser(s.loggingUser))
	s.mux.Handle("POST /logging/notify", s.requireUser(s.loggingNotify))
	s.mux.Handle("GET /admin/logging", s.requireAdmin(s.loggingAdmin))
	s.mux.Handle("POST /admin/logging", s.requireAdmin(s.loggingSet))
}

// levelHelp says in a sentence what each level records.
var levelHelp = map[string]string{
	"off":   "nothing, not even errors",
	"error": "only what failed",
	"warn":  "errors and things that look wrong",
	"info":  "normal operation: starts, links up and down, registrations",
	"debug": "details for a developer: protocol steps; may contain addresses",
	"calls": "one line per call event (started, answered, ended): line, cells, codecs, how it ended; phone numbers are masked except the last three digits. Written by the call log, so calls that are not in the call log yet are not covered",
}

type loggingData struct {
	Rows        []logctl.Row
	Default     string
	Levels      []string
	Help        map[string]string
	Admin       bool
	Notify      bool
	HasEmail    bool
	MailOK      bool
	Subscribers int
}

func (s *Server) loggingView(r *http.Request, u *auth.User) (loggingData, error) {
	d := loggingData{Rows: s.LogCtl.Effective(), Default: s.LogCtl.State().Default, Levels: logctl.Names, Help: levelHelp,
		Admin: u.IsAdmin(), HasEmail: u.Email != "", MailOK: s.mailOn()}
	err := s.Pool.QueryRow(r.Context(), "SELECT notify_log_changes FROM app_user WHERE id = $1", u.ID).Scan(&d.Notify)
	if err == nil && u.IsAdmin() {
		err = s.Pool.QueryRow(r.Context(), "SELECT count(*) FROM app_user WHERE (notify_log_changes OR role = 'admin') AND NOT disabled AND coalesce(email, '') <> ''").Scan(&d.Subscribers)
	}
	return d, err
}

func (s *Server) loggingUser(w http.ResponseWriter, r *http.Request, u *auth.User) {
	d, err := s.loggingView(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "logging.html", page{Title: "Logging", User: u, Data: d, Notice: r.FormValue("done")})
}

func (s *Server) loggingNotify(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	on := r.PostFormValue("notify") == "on"
	if on && u.Email == "" {
		d, _ := s.loggingView(r, u)
		s.render(w, http.StatusBadRequest, "logging.html", page{Title: "Logging", User: u, Data: d, Error: "Your account has no e-mail address. Add one first."})
		return
	}
	if _, err := s.Pool.Exec(r.Context(), "UPDATE app_user SET notify_log_changes = $2 WHERE id = $1", u.ID, on); err != nil {
		s.internalError(w, r, err)
		return
	}
	dest := "/logging?done=Saved."
	if u.IsAdmin() && strings.HasPrefix(r.Referer(), s.selfURL()+"/admin/logging") {
		dest = "/admin/logging?done=Saved."
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) selfURL() string {
	if s.BaseURL == nil {
		return ""
	}
	return s.BaseURL.Scheme + "://" + s.BaseURL.Host
}

func (s *Server) loggingAdmin(w http.ResponseWriter, r *http.Request, u *auth.User) {
	d, err := s.loggingView(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "logging_admin.html", page{Title: "Logging", User: u, Data: d, Notice: r.FormValue("done")})
}

var logTTL = map[string]time.Duration{"": 0, "1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour}

func (s *Server) loggingSet(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	fail := func(msg string) {
		d, _ := s.loggingView(r, u)
		s.render(w, http.StatusBadRequest, "logging_admin.html", page{Title: "Logging", User: u, Data: d, Error: msg})
	}
	before := s.LogCtl.State()
	ttl, ok := logTTL[r.PostFormValue("ttl")]
	if !ok {
		fail("Choose how long a loud level lasts.")
		return
	}
	if def := r.PostFormValue("default"); def != "" {
		if _, err := logctl.ParseLevel(def); err != nil {
			fail(err.Error() + ".")
			return
		}
		if err := s.LogCtl.Set("", def, 0); err != nil {
			fail(err.Error() + ".")
			return
		}
	}
	for _, c := range logctl.Components {
		lv := r.PostFormValue("level_" + c)
		if lv == "" {
			continue
		}
		if lv == "default" {
			s.LogCtl.Clear(c)
			continue
		}
		l, err := logctl.ParseLevel(lv)
		if err != nil {
			fail(err.Error() + ".")
			return
		}
		if lv == r.PostFormValue("orig_"+c) {
			continue // not touched: keep it, and its expiry
		}
		t := ttl
		if l >= slog.LevelInfo { // info and quieter never need an expiry
			t = 0
		}
		if err := s.LogCtl.Set(c, lv, t); err != nil {
			fail(err.Error() + ".")
			return
		}
	}
	s.logChanged(r.Context(), u.Username, before)
	http.Redirect(w, r, "/admin/logging?done=Saved.", http.StatusSeeOther)
}

// logChanged persists the new state, audits it and tells those who asked.
func (s *Server) logChanged(ctx context.Context, who string, before logctl.State) {
	after := s.LogCtl.State()
	if s.Settings != nil {
		if err := s.Settings.Put(ctx, settings.KeyLogging, after); err != nil {
			s.Log.Error("save log levels", "err", err)
		}
	}
	diff := logDiff(before, after)
	if diff == "" {
		return
	}
	_ = audit.Record(ctx, s.Pool, audit.Entry{Action: "log.level", ObjectType: "setting", ObjectID: settings.KeyLogging, Details: map[string]any{"changes": diff}})
	go s.notifyLogChange(who, diff) //nolint:gosec // G118: the mail outlives the request on purpose
}

func logDiff(a, b logctl.State) string {
	var parts []string
	if a.Default != b.Default {
		parts = append(parts, fmt.Sprintf("default: %s → %s", a.Default, b.Default))
	}
	for _, c := range logctl.Components {
		x, y := a.Components[c], b.Components[c]
		if x.Level == y.Level && (x.Until == nil) == (y.Until == nil) {
			continue
		}
		from, to := "default", "default"
		if x.Level != "" {
			from = x.Level
		}
		if y.Level != "" {
			to = y.Level
			if y.Until != nil {
				to += " until " + y.Until.Local().Format("2006-01-02 15:04")
			}
		}
		parts = append(parts, fmt.Sprintf("%s: %s → %s", c, from, to))
	}
	return strings.Join(parts, "; ")
}

// notifyLogChange mails the users who opted in. It runs outside the request,
// so a slow mail server does not hold up the admin's page.
func (s *Server) notifyLogChange(who, diff string) {
	if !s.mailOn() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rows, err := s.Pool.Query(ctx, "SELECT email FROM app_user WHERE (notify_log_changes OR role = 'admin') AND NOT disabled AND coalesce(email, '') <> ''")
	if err != nil {
		s.Log.Error("log notice: list", "err", err)
		return
	}
	var to []string
	for rows.Next() {
		var e string
		if rows.Scan(&e) == nil {
			to = append(to, e)
		}
	}
	rows.Close()
	if len(to) == 0 {
		return
	}
	body := fmt.Sprintf("The logging of %s was changed by %s.\n\n%s\n\nYou get this message because you are an administrator or asked for it on the Logging page.\n", s.brand(ctx).Name, who, diff)
	if err := s.Mailer.Send(ctx, to, "Logging was changed", body); err != nil {
		s.Log.Error("log notice: send", "err", err)
	}
}

// RunLogExpiry returns debug levels to the default when their time is up. Run
// it in a goroutine; it stops with ctx.
func (s *Server) RunLogExpiry(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			before := s.LogCtl.State()
			if got := s.LogCtl.Expire(); len(got) > 0 {
				s.Log.Info("log levels expired", "components", got)
				s.logChanged(audit.WithActor(ctx, audit.Actor{Name: "system"}), "the controller (a time limit ended)", before)
			}
		}
	}
}
