// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

// registrationOpen reports whether the sign-up page is on: an admin turned
// self-registration on and local accounts exist to sign up for.
func (s *Server) registrationOpen(ctx context.Context) bool {
	if s.Settings == nil || !s.LocalLogin {
		return false
	}
	r, err := s.Settings.Registration(ctx)
	return err == nil && r.Enabled
}

type registerData struct {
	Username, Email string
	Approval        bool
	Done            bool
	Pending         bool
}

func (s *Server) registerPage(w http.ResponseWriter, r *http.Request) {
	if auth.UserFrom(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.registrationOpen(r.Context()) {
		s.notFound(w, r, nil)
		return
	}
	pol, _ := s.Settings.Registration(r.Context())
	s.render(w, http.StatusOK, "register.html", page{Title: "Register", Data: registerData{Approval: pol.RequireApproval}})
}

func (s *Server) registerPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if !s.registrationOpen(r.Context()) {
		s.notFound(w, r, nil)
		return
	}
	pol, err := s.Settings.Registration(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	d := registerData{Username: strings.TrimSpace(r.PostFormValue("username")), Email: strings.TrimSpace(r.PostFormValue("email")), Approval: pol.RequireApproval}
	fail := func(status int, msg string) {
		s.render(w, status, "register.html", page{Title: "Register", Error: msg, Data: d})
	}
	if d.Email != "" && auth.ValidEmail(d.Email) == nil && !pol.AllowsEmail(d.Email) {
		fail(http.StatusBadRequest, "Sign-up is limited to certain e-mail domains, and yours is not one of them.")
		return
	}
	u, err := s.Auth.RegisterSelf(r.Context(), auth.SelfRegistration{
		Username: d.Username, Email: d.Email, Password: r.PostFormValue("password"), Pending: pol.RequireApproval,
	}, addrFrom(r.Context()).String())
	switch {
	case errors.Is(err, auth.ErrRegisterRate):
		fail(http.StatusTooManyRequests, "Too many sign-ups from your address. Try again in a few minutes.")
		return
	case errors.Is(err, auth.ErrUsernameTaken):
		fail(http.StatusConflict, "That username is taken.")
		return
	case err != nil:
		fail(http.StatusBadRequest, "Could not sign you up: "+err.Error()+".")
		return
	}
	if pol.RequireApproval {
		s.render(w, http.StatusOK, "register.html", page{Title: "Register", Data: registerData{Done: true, Pending: true}})
		return
	}
	s.startSession(w, r, u, "local", "/")
}

// userApprove lets a pending sign-up in.
func (s *Server) userApprove(w http.ResponseWriter, r *http.Request, u *auth.User) {
	t, ok := s.userByPath(w, r, u)
	if !ok {
		return
	}
	no := false
	if _, err := s.Auth.UpdateUser(r.Context(), u, t.ID, auth.UserUpdate{Disabled: &no}); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
