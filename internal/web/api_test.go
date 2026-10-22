// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
)

// apiClient sends JSON requests and checks every response against
// docs/api/openapi.yaml.
type apiClient struct {
	t      *testing.T
	e      *env
	c      *http.Client
	router routers.Router
}

func newAPIClient(t *testing.T, e *env, c *http.Client) *apiClient {
	b, err := os.ReadFile("../../docs/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromData(b)
	if err != nil {
		t.Fatal(err)
	}
	doc.Servers = openapi3.Servers{{URL: e.srv.URL + apiPrefix}}
	r, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return &apiClient{t: t, e: e, c: c, router: r}
}

func (a *apiClient) do(method, path string, body any, want int) map[string]any {
	a.t.Helper()
	var rd io.Reader
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, a.e.srv.URL+apiPrefix+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := a.c.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		a.t.Fatalf("%s %s: %d, want %d: %s", method, path, resp.StatusCode, want, out)
	}
	// Validate the response against the spec.
	vreq, _ := http.NewRequest(method, a.e.srv.URL+apiPrefix+path, bytes.NewReader(raw))
	route, params, err := a.router.FindRoute(vreq)
	if err != nil {
		a.t.Fatalf("%s %s is not in the spec: %v", method, path, err)
	}
	in := &openapi3filter.RequestValidationInput{Request: vreq, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
	rv := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: resp.StatusCode, Header: resp.Header,
		Options: &openapi3filter.Options{IncludeResponseStatus: true}}
	rv.SetBodyBytes(out)
	if err := openapi3filter.ValidateResponse(context.Background(), rv); err != nil {
		a.t.Errorf("%s %s: response does not match the spec: %v\n%s", method, path, err, out)
	}
	var m map[string]any
	if len(out) > 0 && out[0] == '{' {
		_ = json.Unmarshal(out, &m)
	}
	return m
}

func TestAPI(t *testing.T) {
	e := newEnvWith(t, func(s *Server) {
		ks, _ := secret.GenerateMasterKey()
		mk, _ := secret.ParseMasterKey(ks)
		kr, err := db.LoadKeyring(ctx, s.Pool, mk, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.SIPAccounts, s.SIP = &sipaccount.Store{Pool: s.Pool, Keyring: kr}, fakeSIP{}
		if s.Cells, err = cell.Open(ctx, s.Pool, kr, "dect.example.org:7300"); err != nil {
			t.Fatal(err)
		}
	})
	anon := newAPIClient(t, e, e.browser())
	if m := anon.do("GET", "/me", nil, http.StatusUnauthorized); m["code"] != "unauthenticated" {
		t.Errorf("anonymous: %v", m)
	}
	uc := e.browser()
	e.login(t, uc, "user", "user password")
	user := newAPIClient(t, e, uc)
	ac := e.browser()
	e.login(t, ac, "admin", "admin password")
	admin := newAPIClient(t, e, ac)

	if m := user.do("GET", "/me", nil, http.StatusOK); m["username"] != "user" || m["role"] != "user" {
		t.Errorf("me: %v", m)
	}
	user.do("GET", "/users", nil, http.StatusNotFound)
	admin.do("GET", "/users", nil, http.StatusOK)

	// SIP accounts: create, read, patch, status, delete; secrets never returned.
	acc := user.do("POST", "/sip-accounts", map[string]any{"label": "Home", "username": "301", "domain": "pbx.example.org",
		"password": "very-secret", "transport": "udp", "media_encryption": "optional", "codecs": []string{"G722", "PCMA"}}, http.StatusCreated)
	if acc["has_password"] != true || acc["password"] != nil {
		t.Fatalf("created %v", acc)
	}
	if w, _ := acc["warnings"].([]any); len(w) != 1 || w[0] != "srtp_without_tls" {
		t.Errorf("warnings %v", acc["warnings"])
	}
	id := acc["id"].(string)
	user.do("GET", "/sip-accounts/"+id, nil, http.StatusOK)
	user.do("PATCH", "/sip-accounts/"+id, map[string]any{"label": "Home line"}, http.StatusOK)
	if m := user.do("PATCH", "/sip-accounts/"+id, map[string]any{"codecs": []string{"GSM"}}, http.StatusBadRequest); m["code"] != "invalid" {
		t.Errorf("invalid patch: %v", m)
	}
	user.do("PATCH", "/sip-accounts/"+id, map[string]any{"owner_id": nil}, http.StatusForbidden)
	if m := user.do("GET", "/sip-accounts/"+id+"/status", nil, http.StatusOK); m["registration"] != "registered" {
		t.Errorf("status %v", m)
	}
	if m := user.do("GET", "/sip-accounts", nil, http.StatusOK); len(m["items"].([]any)) != 1 {
		t.Errorf("list %v", m)
	}
	// Someone else's account is not found, for reading and deleting.
	e.store.CreateLocalUser(ctx, newUser("carl"))
	cc := e.browser()
	e.login(t, cc, "carl", "carl password!")
	carl := newAPIClient(t, e, cc)
	carl.do("GET", "/sip-accounts/"+id, nil, http.StatusNotFound)
	carl.do("DELETE", "/sip-accounts/"+id, nil, http.StatusNotFound)
	user.do("DELETE", "/sip-accounts/"+id, nil, http.StatusNoContent)

	// Enrollment tokens and cells.
	user.do("POST", "/enrollment-tokens", map[string]any{"note": "x"}, http.StatusNotFound)
	tok := admin.do("POST", "/enrollment-tokens", map[string]any{"note": "Garage", "ttl_seconds": 3600}, http.StatusCreated)
	if !strings.HasPrefix(tok["enrollment_string"].(string), "opendectcell-enroll:v1?") {
		t.Fatalf("token %v", tok)
	}
	admin.do("GET", "/enrollment-tokens", nil, http.StatusOK)
	admin.do("GET", "/cells", nil, http.StatusOK)
	user.do("GET", "/cells", nil, http.StatusOK)
	admin.do("DELETE", "/enrollment-tokens/"+tok["id"].(string), nil, http.StatusNoContent)

	// Users: create, patch, refuse self-demotion, delete.
	nu := admin.do("POST", "/users", map[string]any{"username": "dana", "password": "dana password!", "role": "user"}, http.StatusCreated)
	admin.do("POST", "/users", map[string]any{"username": "DANA", "password": "dana password!", "role": "user"}, http.StatusConflict)
	admin.do("PATCH", "/users/"+nu["id"].(string), map[string]any{"may_add_cells": true}, http.StatusOK)
	me := admin.do("GET", "/me", nil, http.StatusOK)
	if m := admin.do("PATCH", "/users/"+me["id"].(string), map[string]any{"role": "user"}, http.StatusConflict); m["code"] != "self" {
		t.Errorf("self demote: %v", m)
	}
	admin.do("DELETE", "/users/"+nu["id"].(string), nil, http.StatusNoContent)
	admin.do("GET", "/users/"+nu["id"].(string), nil, http.StatusNotFound)

	if m := admin.do("GET", "/audit?limit=5", nil, http.StatusOK); len(m["items"].([]any)) != 5 || m["next"] == nil {
		t.Errorf("audit page: %v", m)
	}

	// Cross-site writes are refused, as for the pages.
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+apiPrefix+"/sip-accounts", strings.NewReader("{}"))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, err := uc.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site API POST: %v %v", resp, err)
	}
}
