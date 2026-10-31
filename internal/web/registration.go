// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	Verify          bool // an e-mail link is needed (SMTP is configured)
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
	s.render(w, http.StatusOK, "register.html", page{Title: "Register", Data: registerData{Approval: pol.RequireApproval, Verify: s.mailOn()}})
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
	d := registerData{Username: strings.TrimSpace(r.PostFormValue("username")), Email: strings.TrimSpace(r.PostFormValue("email")), Approval: pol.RequireApproval, Verify: s.mailOn()}
	fail := func(status int, msg string) {
		s.render(w, status, "register.html", page{Title: "Register", Error: msg, Data: d})
	}
	if d.Email != "" && auth.ValidEmail(d.Email) == nil && !pol.AllowsEmail(d.Email) {
		fail(http.StatusBadRequest, "Sign-up is limited to certain e-mail domains, and yours is not one of them.")
		return
	}
	u, err := s.Auth.RegisterSelf(r.Context(), auth.SelfRegistration{
		Username: d.Username, Email: d.Email, Password: r.PostFormValue("password"), Pending: pol.RequireApproval, Unverified: d.Verify,
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
	if d.Verify {
		go s.sendVerification(u, d.Email) //nolint:gosec // G118: the mail outlives the request on purpose
		s.render(w, http.StatusOK, "register.html", page{Title: "Register", Data: registerData{Done: true, Verify: true, Pending: pol.RequireApproval, Email: d.Email}})
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

// link returns an absolute link on this controller, for mail.
func (s *Server) link(path string, q url.Values) string {
	base := ""
	if s.BaseURL != nil {
		base = s.BaseURL.Scheme + "://" + s.BaseURL.Host
	}
	if len(q) == 0 {
		return base + path
	}
	return base + path + "?" + q.Encode()
}

// sendMail sends one mail outside the request; a failure is logged, and the
// person can ask again.
func (s *Server) sendMail(to, subject, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.Mailer.Send(ctx, []string{to}, subject, body); err != nil {
		s.Log.Error("mail not sent", "subject", subject, "err", err)
	}
}

func (s *Server) sendVerification(u *auth.User, email string) {
	ctx := context.Background()
	tok, err := s.Auth.NewEmailToken(ctx, u.ID, auth.TokenVerify, auth.VerifyTTL)
	if err != nil {
		s.Log.Error("verification token", "err", err)
		return
	}
	name := s.brand(ctx).Name
	s.sendMail(email, "Confirm your e-mail address", fmt.Sprintf(
		"Hello %s,\n\nsomeone, hopefully you, created an account on %s with this address. To confirm it, open:\n\n%s\n\nThe link works for 24 hours. If you did not sign up, ignore this message; the account is removed unconfirmed.\n",
		u.Username, name, s.link("/register/verify", url.Values{"token": {tok}})))
}

func (s *Server) mailPagesOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.mailOn() || !s.LocalLogin {
		s.notFound(w, r, nil)
		return false
	}
	return true
}

func (s *Server) message(w http.ResponseWriter, status int, title, text string) {
	s.render(w, status, "message.html", page{Title: title, Data: text})
}

// verifyPage asks for confirmation before using the token: a mail scanner
// that fetches the link must not complete the step.
func (s *Server) verifyPage(w http.ResponseWriter, r *http.Request) {
	if !s.mailPagesOn(w, r) {
		return
	}
	tok := r.FormValue("token")
	if _, err := s.Auth.PeekEmailToken(r.Context(), tok, auth.TokenVerify); err != nil {
		s.message(w, http.StatusBadRequest, "Link not valid", "This link is not valid any more. Links work once and for 24 hours. You can ask for a new one on the sign-up page, or register again after two days.")
		return
	}
	s.render(w, http.StatusOK, "verify.html", page{Title: "Confirm your address", Data: tok})
}

func (s *Server) verifyPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if !s.mailPagesOn(w, r) {
		return
	}
	u, err := s.Auth.VerifyEmail(r.Context(), r.PostFormValue("token"))
	if errors.Is(err, auth.ErrBadToken) {
		s.message(w, http.StatusBadRequest, "Link not valid", "This link is not valid any more.")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if u.Disabled { // still waiting for an administrator
		s.message(w, http.StatusOK, "Address confirmed", "Thank you. An administrator still has to approve your account before you can log in.")
		return
	}
	s.message(w, http.StatusOK, "Address confirmed", "Thank you. You can log in now.")
}

// resendPost sends the verification link again. The answer is the same
// whether or not the address has a pending sign-up.
func (s *Server) resendPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if !s.mailPagesOn(w, r) {
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	u, err := s.Auth.RequestVerification(r.Context(), email, addrFrom(r.Context()).String())
	if errors.Is(err, auth.ErrResetRate) {
		s.message(w, http.StatusTooManyRequests, "Too many requests", "Try again in a few minutes.")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if u != nil {
		go s.sendVerification(u, email) //nolint:gosec // G118: as above
	}
	s.message(w, http.StatusOK, "Check your inbox", "If that address has a sign-up waiting for confirmation, a new link is on its way.")
}

func (s *Server) forgotPage(w http.ResponseWriter, r *http.Request) {
	if !s.mailPagesOn(w, r) {
		return
	}
	s.render(w, http.StatusOK, "forgot.html", page{Title: "Forgot your password"})
}

func (s *Server) forgotPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if !s.mailPagesOn(w, r) {
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	u, err := s.Auth.RequestReset(r.Context(), email, addrFrom(r.Context()).String())
	if errors.Is(err, auth.ErrResetRate) {
		s.message(w, http.StatusTooManyRequests, "Too many requests", "Try again in a few minutes.")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if u != nil {
		go func() { //nolint:gosec // G118: as above
			ctx := context.Background()
			tok, err := s.Auth.NewEmailToken(ctx, u.ID, auth.TokenReset, time.Hour)
			if err != nil {
				s.Log.Error("reset token", "err", err)
				return
			}
			s.sendMail(email, "Reset your password", fmt.Sprintf(
				"Hello %s,\n\nto choose a new password for %s, open:\n\n%s\n\nThe link works once, for one hour. If you did not ask for this, ignore this message; your password stays as it is.\n",
				u.Username, s.brand(ctx).Name, s.link("/reset", url.Values{"token": {tok}})))
		}()
	}
	s.message(w, http.StatusOK, "Check your inbox", "If that address belongs to an account, a link to choose a new password is on its way. It works for one hour.")
}

func (s *Server) resetPage(w http.ResponseWriter, r *http.Request) {
	if !s.mailPagesOn(w, r) {
		return
	}
	tok := r.FormValue("token")
	if _, err := s.Auth.PeekEmailToken(r.Context(), tok, auth.TokenReset); err != nil {
		s.message(w, http.StatusBadRequest, "Link not valid", "This link is not valid any more. Ask for a new one.")
		return
	}
	s.render(w, http.StatusOK, "reset.html", page{Title: "Choose a new password", Data: tok})
}

func (s *Server) resetPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if !s.mailPagesOn(w, r) {
		return
	}
	tok := r.PostFormValue("token")
	err := s.Auth.ResetPassword(r.Context(), tok, r.PostFormValue("password"))
	switch {
	case errors.Is(err, auth.ErrBadToken):
		s.message(w, http.StatusBadRequest, "Link not valid", "This link is not valid any more. Ask for a new one.")
	case err != nil && strings.HasPrefix(err.Error(), "password must"):
		s.render(w, http.StatusBadRequest, "reset.html", page{Title: "Choose a new password", Data: tok, Error: "The " + err.Error() + "."})
	case err != nil:
		s.internalError(w, r, err)
	default:
		s.message(w, http.StatusOK, "Password changed", "Your password was changed and you were logged out everywhere. You can log in now.")
	}
}
