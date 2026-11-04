// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

// page is what every template receives.
type page struct {
	Title   string
	User    *auth.User
	Version string
	Error   string
	Notice  string
	Warn    string // a warning that is not an error: the page works, but the user should know
	// PromptTOTP: offer the user two-factor login (filled in by render; never a demand)
	PromptTOTP bool
	// AdminNeeds2FA: an administrator whom the settings require to have two-factor login and who has none; they are a plain
	// user until it is set up, and the page says so (filled in by render)
	AdminNeeds2FA bool
	Data          any

	// filled in by render
	Brand   brand
	Nav     []navGroup
	Section string
	Path    string

	// set by a handler
	Wide             bool // an anonymous page that needs the full width (the public map)
	Leaflet          bool // the page has a map: load Leaflet and say where the tiles come from
	Tiles            string
	TilesAttribution string
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	p.Version = s.Version
	ctx := context.Background()
	if nw, ok := w.(*navWriter); ok {
		ctx, p.Path = nw.r.Context(), nw.r.URL.Path
	}
	p.Brand = s.brand(ctx)
	p.Nav = navFor(p.User, p.Path)
	p.Section = section(p.Nav)
	if p.User != nil && s.Auth != nil && !strings.HasPrefix(p.Path, "/account/2fa") {
		if p.User.Admin2FAPending {
			p.AdminNeeds2FA = true // instead of the offer: this one is not optional, and the admin pages are missing until it is done
		} else {
			p.PromptTOTP = s.Auth.ShouldPromptTOTP(ctx, p.User)
		}
	}
	if p.Leaflet {
		m, _ := s.mapConfig(ctx)
		p.Tiles, p.TilesAttribution = m.TileURL, m.Attribution
	}
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
	Register   bool // self-registration is on
	Forgot     bool // reset by e-mail is possible
}

func (s *Server) loginData(r *http.Request) loginData {
	return loginData{
		Next:       auth.SafeReturnPath(r.FormValue("next")),
		LocalLogin: s.LocalLogin,
		OIDC:       s.OIDC != nil,
		OIDCName:   s.OIDCName,
		Register:   s.registrationOpen(r.Context()),
		Forgot:     s.LocalLogin && s.mailOn(),
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if auth.UserFrom(r.Context()) != nil {
		http.Redirect(w, r, auth.SafeReturnPath(r.FormValue("next")), http.StatusSeeOther) //nolint:gosec // G710: SafeReturnPath allows local paths only
		return
	}
	p := page{Title: "Log in", Data: s.loginData(r)}
	if r.FormValue("erased") == "1" {
		p.Notice = "Your account and data are deleted."
	}
	s.render(w, http.StatusOK, "login.html", p)
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !s.LocalLogin {
		s.notFound(w, r, nil)
		return
	}
	d := s.loginData(r)
	d.Username = r.PostFormValue("username")
	id, err := s.Auth.CheckPassword1(r.Context(), d.Username, r.PostFormValue("password"), addrFrom(r.Context()).String())
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
	if s.Auth.Keyring != nil {
		on, err := s.Auth.HasTOTP(r.Context(), id)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if on && !s.deviceKnown(r, id) {
			tok, err := s.Auth.NewMFAChallenge(r.Context(), id, d.Next)
			if err != nil {
				s.internalError(w, r, err)
				return
			}
			s.render(w, http.StatusOK, "login_2fa.html", page{Title: "Second step", Data: tok})
			return
		}
	}
	u, err := s.Auth.CompleteLogin(r.Context(), id, "local")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.startSession(w, r, u, "local", d.Next)
}

// login2FA is the second step of a login: the code from the authenticator, or
// a recovery code.
func (s *Server) login2FA(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	tok := r.PostFormValue("challenge")
	id, next, err := s.Auth.PeekMFAChallenge(r.Context(), tok)
	if err != nil {
		s.render(w, http.StatusBadRequest, "login.html", page{Title: "Log in", Error: "The login took too long. Please start again.", Data: s.loginData(r)})
		return
	}
	err = s.Auth.VerifySecondFactor(r.Context(), id, r.PostFormValue("code"), addrFrom(r.Context()).String())
	switch {
	case errors.Is(err, auth.ErrBadCode):
		s.render(w, http.StatusUnauthorized, "login_2fa.html", page{Title: "Second step", Data: tok, Error: "That code is not right."})
		return
	case errors.Is(err, auth.ErrRateLimited):
		s.render(w, http.StatusTooManyRequests, "login_2fa.html", page{Title: "Second step", Data: tok, Error: "Too many wrong codes. Try again in a few minutes."})
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	_ = s.Auth.DropMFAChallenge(r.Context(), tok)
	u, err := s.Auth.CompleteLogin(r.Context(), id, "local")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if r.PostFormValue("remember") == "on" {
		s.rememberDevice(w, r, id)
	}
	s.startSession(w, r, u, "local", next)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *auth.User, method, next string) {
	// A fresh token at every login: an attacker-planted cookie never
	// becomes an authenticated session (session fixation).
	if old := sessionFrom(r.Context()); old != nil {
		_ = s.Auth.DeleteSession(r.Context(), old.TokenHash) // the new token is what matters
	}
	tok, err := s.Auth.CreateSessionOn(r.Context(), u, method, deviceLabel(r.UserAgent()))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.setCookie(w, tok)
	http.Redirect(w, r, auth.SafeReturnPath(next), http.StatusSeeOther) //nolint:gosec // G710: SafeReturnPath allows local paths only
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if sess := sessionFrom(r.Context()); sess != nil {
		if err := s.Auth.DeleteSession(r.Context(), sess.TokenHash); err != nil {
			s.internalError(w, r, err)
			return
		}
		_ = audit.Record(r.Context(), s.Pool, audit.Entry{Action: "user.logout", ObjectType: "user", ObjectID: sess.User.ID.String()})
	}
	s.clearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// oidcBindCookie holds the token that ties an OIDC login to the browser that started it.
const oidcBindCookie = "dw_oidc_bind"

func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		s.notFound(w, r, nil)
		return
	}
	u, bind, err := s.OIDC.Begin(r.Context(), r.FormValue("next"))
	if err == nil {
		// binds the login to this browser; SameSite=Lax still sends it on the IdP's redirect back
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the base URL, as for the session cookie
			Name: oidcBindCookie, Value: bind, Path: "/auth/oidc", HttpOnly: true, Secure: s.SecureCookies,
			SameSite: http.SameSiteLaxMode, MaxAge: 600})
	}
	if err != nil {
		s.Log.Error("oidc start", "err", err)
		s.render(w, http.StatusBadGateway, "login.html", page{Title: "Log in", Error: "The sign-on provider is not reachable. Try again later.", Data: s.loginData(r)})
		return
	}
	http.Redirect(w, r, u, http.StatusFound) //nolint:gosec // G710: u is the configured IdP's authorisation endpoint
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
	bind := ""
	if c, err := r.Cookie(oidcBindCookie); err == nil {
		bind = c.Value
	}
	// one login, one try: the cookie goes whatever the result
	http.SetCookie(w, &http.Cookie{Name: oidcBindCookie, Value: "", Path: "/auth/oidc", MaxAge: -1, HttpOnly: true, //nolint:gosec // G124: as above
		Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode})
	u, next, err := s.OIDC.Callback(r.Context(), r.FormValue("state"), r.FormValue("code"), bind)
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

type homeData struct {
	Summary  stats.Summary
	HasStats bool
	Stations int
	Online   int
	Steps    []homeStep
}

// homeStep is one line of the getting-started list.
type homeStep struct {
	Done  bool
	Text  string
	Href  string
	Label string
}

func (s *Server) home(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var d homeData
	if s.Stats != nil {
		if m, err := s.Stats.Summary(r.Context(), stats.Own(u.ID)); err == nil {
			d.Summary, d.HasStats = m, true
		}
	}
	if s.Cells != nil {
		if cells, err := s.Cells.ListOwned(r.Context(), u); err == nil {
			for _, c := range cells {
				d.Stations++
				if s.liveStatus(c.ID).Online {
					d.Online++
				}
			}
		}
	}
	d.Steps = []homeStep{
		{Done: d.Stations > 0, Text: "A base station is connected", Href: "/stations", Label: "Base stations"},
		{Done: d.Summary.SIPAccounts > 0, Text: "A VoIP line (SIP account) is set up", Href: "/sip-accounts", Label: "VoIP lines"},
		{Done: d.Summary.Handsets > 0, Text: "A handset is registered", Href: "/handsets", Label: "Handsets"},
	}
	s.render(w, http.StatusOK, "home.html", page{Title: "Overview", User: u, Data: d})
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	d, err := s.notifState(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "account.html", page{Title: "Account", User: u, Data: d, Notice: r.FormValue("done")})
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

type callsData struct {
	Rows       []calllog.Call
	NextBefore int64
}

// adminCalls lists recent calls with their media quality on both legs
// (docs/media-engine.md §7).
func (s *Server) adminCalls(w http.ResponseWriter, r *http.Request, u *auth.User) {
	f := calllog.Filter{Limit: 100}
	if b, err := strconv.ParseInt(r.FormValue("before"), 10, 64); err == nil {
		f.Before = b
	}
	rows, err := (&calllog.Store{Pool: s.Pool}).List(r.Context(), f)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	d := callsData{Rows: rows}
	if len(rows) == f.Limit {
		d.NextBefore = rows[len(rows)-1].ID
	}
	s.render(w, http.StatusOK, "admin_calls.html", page{Title: "Calls", User: u, Data: d})
}
