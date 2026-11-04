// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"strings"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

// deviceLabel is a short description of the browser behind a User-Agent header, "Firefox on Linux": enough for a user to
// tell their sessions apart, and nothing a tracker could use. "" if the header says nothing recognisable.
func deviceLabel(ua string) string {
	browser := ""
	for _, b := range []struct{ marker, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Chromium/", "Chromium"}, {"Safari/", "Safari"},
	} {
		if strings.Contains(ua, b.marker) {
			browser = b.name
			break
		}
	}
	system := ""
	for _, o := range []struct{ marker, name string }{
		{"Android", "Android"}, {"iPhone", "iOS"}, {"iPad", "iOS"}, {"Windows", "Windows"}, {"Mac OS X", "macOS"}, {"CrOS", "ChromeOS"},
		{"Linux", "Linux"}, {"X11", "Linux"},
	} {
		if strings.Contains(ua, o.marker) {
			system = o.name
			break
		}
	}
	switch {
	case browser != "" && system != "":
		return browser + " on " + system
	case browser != "":
		return browser
	}
	return system
}

type sessionsData struct{ Sessions []auth.SessionInfo }

func (s *Server) sessionsPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	cur := sessionFrom(r.Context())
	if cur == nil { // an API credential has no session to list
		s.notFound(w, r, u)
		return
	}
	list, err := s.Auth.ListSessions(r.Context(), u.ID, cur.TokenHash)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "sessions.html", page{Title: "Sessions", User: u, Data: sessionsData{list}, Notice: r.FormValue("done")})
}

func (s *Server) sessionEnd(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	cur := sessionFrom(r.Context())
	if cur == nil {
		s.notFound(w, r, u)
		return
	}
	if _, err := s.Auth.EndSession(r.Context(), u.ID, r.PostFormValue("id"), cur.TokenHash); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account/sessions?done=Session+ended.", http.StatusSeeOther)
}

func (s *Server) sessionEndOthers(w http.ResponseWriter, r *http.Request, u *auth.User) {
	cur := sessionFrom(r.Context())
	if cur == nil {
		s.notFound(w, r, u)
		return
	}
	n, err := s.Auth.EndOtherSessions(r.Context(), u.ID, cur.TokenHash)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	msg := "No+other+session+was+open."
	if n > 0 {
		msg = "Every+other+session+is+ended."
	}
	http.Redirect(w, r, "/account/sessions?done="+msg, http.StatusSeeOther)
}
