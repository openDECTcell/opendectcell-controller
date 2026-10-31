// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

type twoFAData struct {
	Available bool // local account, and a keyring to protect the secret
	On        bool
	Left      int // recovery codes not used yet
	Secret    string
	URI       string
	Codes     []string
}

func (s *Server) twoFAState(r *http.Request, u *auth.User) (twoFAData, error) {
	d := twoFAData{Available: u.AuthSource == "local" && s.Auth.Keyring != nil}
	if !d.Available {
		return d, nil
	}
	var err error
	if d.On, err = s.Auth.HasTOTP(r.Context(), u.ID); err == nil && d.On {
		d.Left, err = s.Auth.RecoveryLeft(r.Context(), u.ID)
	}
	return d, err
}

func (s *Server) twoFAPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	d, err := s.twoFAState(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	p := page{Title: "Two-factor login", User: u, Data: d, Notice: r.FormValue("done")}
	if r.FormValue("need") != "" && s.adminNeeds2FA(r, u) {
		p.Error = "Administrators need two-factor login. Set it up here to reach the admin pages."
	}
	s.render(w, http.StatusOK, "twofa.html", p)
}

func (s *Server) twoFAStart(w http.ResponseWriter, r *http.Request, u *auth.User) {
	d, err := s.twoFAState(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !d.Available || d.On {
		s.notFound(w, r, u)
		return
	}
	d.Secret, err = s.Auth.BeginTOTP(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	d.URI = auth.TOTPURI(s.brand(r.Context()).Name, u.Username, d.Secret)
	s.render(w, http.StatusOK, "twofa.html", page{Title: "Two-factor login", User: u, Data: d})
}

func (s *Server) twoFAConfirm(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	d, err := s.twoFAState(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !d.Available || d.On {
		s.notFound(w, r, u)
		return
	}
	codes, err := s.Auth.ConfirmTOTP(r.Context(), u.ID, r.PostFormValue("code"))
	if errors.Is(err, auth.ErrBadCode) {
		// the secret is shown again: the user may not have scanned it yet
		d.Secret, err = s.Auth.BeginTOTP(r.Context(), u.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		d.URI = auth.TOTPURI(s.brand(r.Context()).Name, u.Username, d.Secret)
		s.render(w, http.StatusBadRequest, "twofa.html", page{Title: "Two-factor login", User: u, Data: d, Error: "That code is not right. A new key was made: set it up again and give the current code."})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	d.On, d.Codes, d.Left = true, codes, len(codes)
	s.render(w, http.StatusOK, "twofa.html", page{Title: "Two-factor login", User: u, Data: d, Notice: "Two-factor login is on."})
}

func (s *Server) twoFADisable(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	d, err := s.twoFAState(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !d.Available || !d.On {
		s.notFound(w, r, u)
		return
	}
	if err := s.Auth.CheckPassword(r.Context(), u.ID, r.PostFormValue("password"), addrFrom(r.Context()).String()); err != nil {
		msg := "The password is wrong."
		code := http.StatusBadRequest
		if errors.Is(err, auth.ErrRateLimited) {
			msg, code = "Too many failed attempts. Try again in a few minutes.", http.StatusTooManyRequests
		}
		s.render(w, code, "twofa.html", page{Title: "Two-factor login", User: u, Data: d, Error: msg})
		return
	}
	if err := s.Auth.DisableTOTP(r.Context(), u.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account/2fa?done=Two-factor+login+is+off.", http.StatusSeeOther)
}

// adminNeeds2FA reports whether u is an admin with a local account who must
// set up two-factor login before using the admin pages. Single sign-on admins
// are the provider's business. On a failed lookup the answer is no: a database
// hiccup must not lock the admins out.
func (s *Server) adminNeeds2FA(r *http.Request, u *auth.User) bool {
	if !u.IsAdmin() || u.AuthSource != "local" || s.Auth.Keyring == nil || s.Settings == nil {
		return false
	}
	sec, err := s.Settings.Security(r.Context())
	if err != nil || !sec.RequireAdmin2FA {
		return false
	}
	on, err := s.Auth.HasTOTP(r.Context(), u.ID)
	return err == nil && !on
}
