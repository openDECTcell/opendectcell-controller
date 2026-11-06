// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/openDECTcell/opendectcell-controller/internal/apitoken"
	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

// apiScopes says which scope a personal API token needs for each route of the API (docs/api/openapi.yaml). A route that is not here
// cannot be used with a token at all: the administrators' API, the enrolment of base stations, organizations and the trust setting need
// a session. Adding a route without a decision here fails a test (TestEveryAPIRouteHasAScopeDecision): the default is no.
var apiScopes = map[string]string{
	"GET /api/v1/me": "profile:read",

	"GET /api/v1/handsets":                   "handsets:read",
	"GET /api/v1/handsets/{id}":              "handsets:read",
	"PATCH /api/v1/handsets/{id}":            "handsets:write",
	"GET /api/v1/handsets/{id}/lists/{list}": "handsets:read",

	"GET /api/v1/sip-accounts":             "lines:read",
	"GET /api/v1/sip-accounts/{id}":        "lines:read",
	"GET /api/v1/sip-accounts/{id}/status": "lines:read",
	"POST /api/v1/sip-accounts":            "lines:write",
	"PATCH /api/v1/sip-accounts/{id}":      "lines:write",
	"DELETE /api/v1/sip-accounts/{id}":     "lines:write",

	"GET /api/v1/contacts":         "contacts:read",
	"GET /api/v1/contacts/{id}":    "contacts:read",
	"POST /api/v1/contacts":        "contacts:write",
	"PUT /api/v1/contacts/{id}":    "contacts:write",
	"DELETE /api/v1/contacts/{id}": "contacts:write",

	"GET /api/v1/calls":              "calls:read",
	"GET /api/v1/calls/{id}":         "calls:read",
	"POST /api/v1/calls/{id}/read":   "calls:write",
	"GET /api/v1/statistics/summary": "statistics:read",

	"GET /api/v1/cells": "stations:read",

	"POST /api/v1/registration-windows":        "registration:write",
	"GET /api/v1/registration-windows/{id}":    "registration:write",
	"DELETE /api/v1/registration-windows/{id}": "registration:write",
}

type tokenKey struct{}

// tokenFrom is the API token a request was made with, if it was made with one.
func tokenFrom(ctx context.Context) (apitoken.Token, bool) {
	t, ok := ctx.Value(tokenKey{}).(apitoken.Token)
	return t, ok
}

// withToken authenticates a request to the API that carries "Authorization: Bearer odc_...": the request is then made by the token's
// user, and only what the token's scopes allow (apiUser checks that). A bad token is refused at once, also if the request carries a
// valid session cookie as well: what a script sends is what it means. Other paths, and other schemes, are not looked at.
func (s *Server) withToken(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth0 := r.Header.Get("Authorization")
		if s.APITokens == nil || !strings.HasPrefix(r.URL.Path, apiPrefix+"/") || len(auth0) < 7 || !strings.EqualFold(auth0[:7], "bearer ") {
			h.ServeHTTP(w, r)
			return
		}
		invalid := func() {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			problem(w, http.StatusUnauthorized, "invalid_token", "the token is not valid, or it has expired or been revoked", nil)
		}
		tok, err := s.APITokens.Authenticate(r.Context(), strings.TrimSpace(auth0[7:]), addrFrom(r.Context()).String())
		switch {
		case errors.Is(err, apitoken.ErrInvalid):
			invalid()
			return
		case err != nil:
			s.Log.Error("api token", "err", err)
			problem(w, http.StatusInternalServerError, "internal", "", nil)
			return
		}
		user, err := s.Auth.UserByID(r.Context(), s.Pool, tok.UserID)
		if err != nil || user.Disabled || user.PendingApproval {
			invalid()
			return
		}
		user = s.withLegal(r.Context(), user)
		ctx := auth.WithUser(r.Context(), user)
		a := user.Actor()
		a.Addr = addrFrom(ctx)
		a.Name += " (token " + tok.Name + ")"
		ctx = audit.WithActor(ctx, a)
		ctx = context.WithValue(ctx, tokenKey{}, tok)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// tokenAllows is the check of apiUser for a request made with a token: nil if the route's scope is one of the token's. It writes the refusal.
func tokenAllows(w http.ResponseWriter, r *http.Request, tok apitoken.Token) bool {
	need, known := apiScopes[r.Pattern]
	switch {
	case !known:
		problem(w, http.StatusForbidden, "token_not_allowed", "this part of the API needs a session: a token cannot use it", nil)
		return false
	case !tok.Has(need):
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+need+`"`)
		problem(w, http.StatusForbidden, "insufficient_scope", "the token lacks the scope "+need, map[string]string{"scope": need})
		return false
	}
	return true
}
