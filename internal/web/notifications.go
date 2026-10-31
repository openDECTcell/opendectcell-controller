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
	Log, Cells bool
	HasEmail   bool
	MailOK     bool
}

func (s *Server) notifState(r *http.Request, u *auth.User) (notifData, error) {
	d := notifData{HasEmail: u.Email != "", MailOK: s.mailOn()}
	err := s.Pool.QueryRow(r.Context(), "SELECT notify_log_changes, notify_cell_alerts FROM app_user WHERE id = $1", u.ID).Scan(&d.Log, &d.Cells)
	return d, err
}

func (s *Server) notificationsSave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	logOn := r.PostFormValue("log") == "on"
	cellsOn := r.PostFormValue("cells") == "on" && u.IsAdmin()
	if (logOn || cellsOn) && u.Email == "" {
		s.render(w, http.StatusBadRequest, "account.html", page{Title: "Account", User: u, Error: "Your account has no e-mail address. Ask an administrator to add one."})
		return
	}
	if _, err := s.Pool.Exec(r.Context(), "UPDATE app_user SET notify_log_changes = $2, notify_cell_alerts = $3 WHERE id = $1", u.ID, logOn, cellsOn); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?done=Saved.", http.StatusSeeOther)
}
