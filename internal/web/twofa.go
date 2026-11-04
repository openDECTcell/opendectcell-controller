// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"html/template"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

type twoFAData struct {
	Available bool // local account, and a keyring to protect the secret
	On        bool
	Left      int // recovery codes not used yet
	Secret    string
	URI       template.URL  // the otpauth link (html/template would turn that scheme into #ZgotmplZ)
	QR        template.HTML // URI as a QR code
	Devices   int           // devices that are not asked for a code at the moment
	Codes     []string
}

func (s *Server) twoFAState(r *http.Request, u *auth.User) (twoFAData, error) {
	d := twoFAData{Available: u.AuthSource == "local" && s.Auth.Keyring != nil}
	if !d.Available {
		return d, nil
	}
	var err error
	if d.On, err = s.Auth.HasTOTP(r.Context(), u.ID); err == nil && d.On {
		if d.Left, err = s.Auth.RecoveryLeft(r.Context(), u.ID); err == nil {
			d.Devices, err = s.Auth.DeviceCount(r.Context(), u.ID)
		}
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
	if u.Admin2FAPending {
		p.Warn = "Administrators need two-factor login. Set it up here to reach the administration pages."
	}
	s.render(w, http.StatusOK, "twofa.html", p)
}

// setupData fills in what the setup page shows of the secret: the link and the QR code that carry it.
func (s *Server) setupData(r *http.Request, u *auth.User, d *twoFAData) {
	uri := auth.TOTPURI(s.brand(r.Context()).Name, u.Username, d.Secret)
	d.URI = template.URL(uri) //nolint:gosec // G203: built by TOTPURI from the instance name, the user name and a base32 key, all escaped
	if q, err := qrSVG(uri); err == nil {
		d.QR = q
	} else {
		s.Log.Warn("QR code not made", "err", err) // the key and the link are still shown
	}
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
	s.setupData(r, u, &d)
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
		s.setupData(r, u, &d)
		s.render(w, http.StatusBadRequest, "twofa.html", page{Title: "Two-factor login", User: u, Data: d, Error: "That code is not right. A new key was made: set it up again and give the current code."})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	d.On, d.Codes, d.Left = true, codes, len(codes)
	s.rememberDevice(w, r, u.ID) // they just proved they hold the app: this browser is not asked at once
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

// deviceCookieName is the cookie that marks a browser the second step has been passed on.
func (s *Server) deviceCookieName() string {
	if s.SecureCookies {
		return "__Host-opendectcell_device"
	}
	return "opendectcell_device"
}

// deviceCookieMaxAge outlives the three weeks a device counts for, so that an old device is recognised
// (and asked) rather than taken for a new one; the database forgets it after six months.
const deviceCookieMaxAge = 365 * 24 * time.Hour

// rememberDevice notes that a code was entered in this browser and gives it the cookie that says so.
func (s *Server) rememberDevice(w http.ResponseWriter, r *http.Request, user uuid.UUID) {
	old := ""
	if c, err := r.Cookie(s.deviceCookieName()); err == nil {
		old = c.Value
	}
	tok, err := s.Auth.RememberDevice(r.Context(), user, old, r.UserAgent())
	if err != nil {
		s.Log.Warn("device not remembered", "err", err) // the user is asked again next time
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the base URL, as for the session cookie
		Name: s.deviceCookieName(), Value: tok, Path: "/", MaxAge: int(deviceCookieMaxAge / time.Second),
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode})
}

// deviceKnown reports whether this browser passed the second step for user within three weeks.
func (s *Server) deviceKnown(r *http.Request, user uuid.UUID) bool {
	c, err := r.Cookie(s.deviceCookieName())
	if err != nil {
		return false
	}
	known, err := s.Auth.DeviceKnown(r.Context(), user, c.Value)
	return err == nil && known
}

// twoFAForgetDevices makes every browser of the user ask for a code at its next login.
func (s *Server) twoFAForgetDevices(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if err := s.Auth.ForgetDevices(r.Context(), u.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account/2fa?done=Every+device+will+ask+for+a+code+at+its+next+login.", http.StatusSeeOther)
}

// twoFALater puts off the offer to set two-factor login up, for a month.
func (s *Server) twoFALater(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := s.Auth.DismissTOTPPrompt(r.Context(), u.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, auth.SafeReturnPath(r.PostFormValue("next")), http.StatusSeeOther) //nolint:gosec // G710: SafeReturnPath allows local paths only
}
