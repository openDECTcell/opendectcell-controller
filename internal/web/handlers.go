// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

// page is what every template receives.
type page struct {
	Title   string
	User    *auth.User
	Version string
	Error   string
	Notice  string
	Data    any
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	p.Version = s.Version
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, p); err != nil {
		s.Log.Error("render", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.render(w, http.StatusNotFound, "error.html", page{Title: "Not found", User: u, Error: "This page does not exist, or you do not have access to it."})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	s.render(w, http.StatusInternalServerError, "error.html", page{Title: "Error", User: auth.UserFrom(r.Context()), Error: "Something went wrong. The error has been logged."})
}

type loginData struct {
	Next       string
	Username   string
	LocalLogin bool
	OIDC       bool
	OIDCName   string
}

func (s *Server) loginData(r *http.Request) loginData {
	return loginData{
		Next:       auth.SafeReturnPath(r.FormValue("next")),
		LocalLogin: s.LocalLogin,
		OIDC:       s.OIDC != nil,
		OIDCName:   s.OIDCName,
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if auth.UserFrom(r.Context()) != nil {
		http.Redirect(w, r, auth.SafeReturnPath(r.FormValue("next")), http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login.html", page{Title: "Log in", Data: s.loginData(r)})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !s.LocalLogin {
		s.notFound(w, r, nil)
		return
	}
	d := s.loginData(r)
	d.Username = r.PostFormValue("username")
	u, err := s.Auth.LoginLocal(r.Context(), d.Username, r.PostFormValue("password"), addrFrom(r.Context()).String())
	switch {
	case errors.Is(err, auth.ErrBadLogin):
		s.render(w, http.StatusUnauthorized, "login.html", page{Title: "Log in", Error: "Invalid username or password.", Data: d})
		return
	case errors.Is(err, auth.ErrRateLimited):
		s.render(w, http.StatusTooManyRequests, "login.html", page{Title: "Log in", Error: "Too many failed attempts. Try again in a few minutes.", Data: d})
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.startSession(w, r, u, "local", d.Next)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *auth.User, method, next string) {
	// A fresh token at every login: an attacker-planted cookie never
	// becomes an authenticated session (session fixation).
	if old := sessionFrom(r.Context()); old != nil {
		s.Auth.DeleteSession(r.Context(), old.TokenHash)
	}
	tok, err := s.Auth.CreateSession(r.Context(), u, method)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.setCookie(w, tok)
	http.Redirect(w, r, auth.SafeReturnPath(next), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if sess := sessionFrom(r.Context()); sess != nil {
		if err := s.Auth.DeleteSession(r.Context(), sess.TokenHash); err != nil {
			s.internalError(w, r, err)
			return
		}
		audit.Record(r.Context(), s.Pool, audit.Entry{Action: "user.logout", ObjectType: "user", ObjectID: sess.User.ID.String()})
	}
	s.clearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		s.notFound(w, r, nil)
		return
	}
	u, err := s.OIDC.Begin(r.Context(), r.FormValue("next"))
	if err != nil {
		s.Log.Error("oidc start", "err", err)
		s.render(w, http.StatusBadGateway, "login.html", page{Title: "Log in", Error: "The sign-on provider is not reachable. Try again later.", Data: s.loginData(r)})
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		s.notFound(w, r, nil)
		return
	}
	d := s.loginData(r)
	if e := r.FormValue("error"); e != "" {
		s.render(w, http.StatusUnauthorized, "login.html", page{Title: "Log in", Error: "Sign-on was cancelled or refused by the provider.", Data: d})
		return
	}
	u, next, err := s.OIDC.Callback(r.Context(), r.FormValue("state"), r.FormValue("code"))
	switch {
	case errors.Is(err, auth.ErrNotAuthorized):
		s.render(w, http.StatusForbidden, "login.html", page{Title: "Log in", Error: "Your account is not authorised for this controller. Ask an administrator.", Data: d})
		return
	case errors.Is(err, auth.ErrOIDCState):
		s.render(w, http.StatusBadRequest, "login.html", page{Title: "Log in", Error: "The sign-on took too long or was already used. Please try again.", Data: d})
		return
	case err != nil:
		s.Log.Error("oidc callback", "err", err)
		s.render(w, http.StatusBadGateway, "login.html", page{Title: "Log in", Error: "Sign-on failed. The error has been logged.", Data: d})
		return
	}
	s.startSession(w, r, u, "oidc", next)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.render(w, http.StatusOK, "home.html", page{Title: "Overview", User: u})
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.render(w, http.StatusOK, "account.html", page{Title: "Account", User: u})
}

func (s *Server) accountPassword(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	p := page{Title: "Account", User: u}
	if u.AuthSource != "local" {
		p.Error = "Your password is managed by your sign-on provider."
		s.render(w, http.StatusBadRequest, "account.html", p)
		return
	}
	cur, n1, n2 := r.PostFormValue("current"), r.PostFormValue("new"), r.PostFormValue("confirm")
	if err := s.Auth.CheckPassword(r.Context(), u.ID, cur, addrFrom(r.Context()).String()); err != nil {
		p.Error = "The current password is wrong."
		if errors.Is(err, auth.ErrRateLimited) {
			p.Error = "Too many failed attempts. Try again in a few minutes."
		}
		s.render(w, http.StatusBadRequest, "account.html", p)
		return
	}
	if n1 != n2 {
		p.Error = "The new passwords do not match."
		s.render(w, http.StatusBadRequest, "account.html", p)
		return
	}
	if err := auth.CheckNewPassword(n1); err != nil {
		p.Error = "The new " + err.Error() + "."
		s.render(w, http.StatusBadRequest, "account.html", p)
		return
	}
	if err := s.Auth.SetPassword(r.Context(), u.ID, n1, sessionFrom(r.Context()).TokenHash); err != nil {
		s.internalError(w, r, err)
		return
	}
	p.Notice = "Password changed. Your other sessions have been logged out."
	s.render(w, http.StatusOK, "account.html", p)
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request, u *auth.User) {
	users, err := s.Auth.ListUsers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "admin_users.html", page{Title: "Users", User: u, Data: users})
}

type auditData struct {
	Rows       []audit.Row
	NextBefore int64
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request, u *auth.User) {
	f := audit.Filter{ObjectType: r.FormValue("type"), ObjectID: r.FormValue("id"), Limit: 100}
	if b, err := strconv.ParseInt(r.FormValue("before"), 10, 64); err == nil {
		f.Before = b
	}
	rows, err := audit.List(r.Context(), s.Pool, f)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	d := auditData{Rows: rows}
	if len(rows) == f.Limit {
		d.NextBefore = rows[len(rows)-1].ID
	}
	s.render(w, http.StatusOK, "admin_audit.html", page{Title: "Audit log", User: u, Data: d})
}
