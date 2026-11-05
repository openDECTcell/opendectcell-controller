// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/apitoken"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

func (s *Server) apiTokenRoutes() {
	if s.APITokens == nil {
		return
	}
	s.mux.Handle("GET /account/tokens", s.requireUser(s.tokensPage))
	s.mux.Handle("POST /account/tokens", s.requireUser(s.tokenCreate))
	s.mux.Handle("POST /account/tokens/{id}/revoke", s.requireUser(s.tokenRevoke))
}

type tokenRow struct {
	apitoken.Record
	Active bool
	Status string // active, expired, revoked
}

type tokensView struct {
	Tokens   []tokenRow
	Scopes   []apitoken.ScopeInfo
	Local    bool
	New      string // the token just made: shown this once
	NewName  string
	Lifetime []lifetime
	Checked  map[string]bool // the scopes ticked when the form comes back with an error
	Name     string
	Max      int
}

type lifetime struct {
	Days    int
	Label   string
	Default bool
}

var lifetimes = []lifetime{{30, "30 days", false}, {90, "90 days", true}, {180, "180 days", false}, {365, "1 year", false}}

func (s *Server) tokensView(r *http.Request, u *auth.User) (tokensView, error) {
	recs, err := s.APITokens.List(r.Context(), u.ID)
	if err != nil {
		return tokensView{}, err
	}
	v := tokensView{Scopes: apitoken.Scopes, Local: u.AuthSource == "local", Lifetime: lifetimes, Checked: map[string]bool{}, Max: apitoken.MaxPerUser}
	now := time.Now()
	for _, rec := range recs {
		row := tokenRow{Record: rec, Active: rec.Active(now), Status: "active"}
		switch {
		case rec.RevokedAt != nil:
			row.Status = "revoked"
		case !row.Active:
			row.Status = "expired"
		}
		v.Tokens = append(v.Tokens, row)
	}
	return v, nil
}

func (s *Server) tokensPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	v, err := s.tokensView(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "account_tokens.html", page{Title: "API tokens", User: u, Data: v, Notice: r.FormValue("done")})
}

// tokenCreate makes a token. It needs the user's password (or, for single sign-on, their user name): a token is a key to the account that
// works without a session, so a session someone walked away from must not be enough to make one. The token is shown once, on the page
// that answers, and nowhere else.
func (s *Server) tokenCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	v, err := s.tokensView(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	scopes := []string{}
	for _, sc := range apitoken.Scopes {
		if r.PostFormValue("scope_"+sc.Name) == "on" {
			scopes = append(scopes, sc.Name)
			v.Checked[sc.Name] = true
		}
	}
	v.Name = r.PostFormValue("name")
	fail := func(status int, msg string) {
		s.render(w, status, "account_tokens.html", page{Title: "API tokens", User: u, Data: v, Error: msg})
	}
	days, _ := strconv.Atoi(r.PostFormValue("days"))
	if !ok(days, lifetimes) {
		days = 90
	}
	if msg := s.confirmUser(r, u); msg != "" {
		fail(http.StatusBadRequest, msg)
		return
	}
	tok, rec, err := s.APITokens.Create(r.Context(), u.ID, v.Name, scopes, time.Duration(days)*24*time.Hour)
	var verr apitoken.ValidationError
	switch {
	case errors.As(err, &verr):
		fail(http.StatusBadRequest, verr.Error())
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	if v, err = s.tokensView(r, u); err != nil {
		s.internalError(w, r, err)
		return
	}
	v.New, v.NewName = tok, rec.Name
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "account_tokens.html", page{Title: "API tokens", User: u, Data: v})
}

func ok(days int, from []lifetime) bool {
	for _, l := range from {
		if l.Days == days {
			return true
		}
	}
	return false
}

func (s *Server) tokenRevoke(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err == nil {
		err = s.APITokens.Revoke(r.Context(), u.ID, id)
	}
	switch {
	case errors.Is(err, apitoken.ErrNotFound), id == uuid.Nil:
		s.notFound(w, r, u)
	case err != nil:
		s.internalError(w, r, err)
	default:
		http.Redirect(w, r, "/account/tokens?done=Token+revoked.", http.StatusSeeOther)
	}
}
