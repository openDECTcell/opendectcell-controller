// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

func (s *Server) notificationRoutes() {
	s.mux.Handle("POST /account/notifications", s.requireUser(s.notificationsSave))
}

type notifData struct {
	Log, Cells, Missed bool
	HasEmail           bool
	MailOK             bool
	Alerts             alertPrefsView
}

func (s *Server) notifState(r *http.Request, u *auth.User) (notifData, error) {
	d := notifData{HasEmail: u.Email != "", MailOK: s.mailOn()}
	if err := s.Pool.QueryRow(r.Context(), "SELECT notify_log_changes, notify_cell_alerts, notify_missed_calls FROM app_user WHERE id = $1", u.ID).Scan(&d.Log, &d.Cells, &d.Missed); err != nil {
		return d, err
	}
	var err error
	d.Alerts, err = s.alertPrefsFor(r.Context(), u)
	return d, err
}

func (s *Server) notificationsSave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	logOn := r.PostFormValue("log") == "on"
	cellsOn := r.PostFormValue("cells") == "on" && u.IsAdmin()
	missedOn := r.PostFormValue("missed") == "on"
	if (logOn || cellsOn || missedOn) && u.Email == "" {
		s.render(w, http.StatusBadRequest, "account.html", page{Title: "Account", User: u, Error: "Your account has no e-mail address. Ask an administrator to add one."})
		return
	}
	was := false
	if err := s.Pool.QueryRow(r.Context(), "SELECT notify_missed_calls FROM app_user WHERE id = $1", u.ID).Scan(&was); err != nil {
		s.internalError(w, r, err)
		return
	}
	if _, err := s.Pool.Exec(r.Context(), "UPDATE app_user SET notify_log_changes = $2, notify_cell_alerts = $3, notify_missed_calls = $4 WHERE id = $1", u.ID, logOn, cellsOn, missedOn); err != nil {
		s.internalError(w, r, err)
		return
	}
	if missedOn && !was {
		// turning it on is about the calls to come, not the ones already missed
		if _, err := s.Pool.Exec(r.Context(), `UPDATE call_log SET missed_notified_at = now() WHERE owner_user_id = $1 AND direction = 'in'
			AND answered_at IS NULL AND missed_notified_at IS NULL`, u.ID); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	http.Redirect(w, r, "/account?done=Saved.", http.StatusSeeOther)
}
