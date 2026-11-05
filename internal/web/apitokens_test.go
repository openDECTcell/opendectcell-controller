// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/apitoken"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

// The routes of the API that a token can never use. Every other route of the API must have a scope in apiScopes: a new route is
// refused to tokens until somebody has decided, here or there.
var sessionOnlyRoutes = []string{
	"GET /api/v1/users", "POST /api/v1/users", "GET /api/v1/users/{id}", "PATCH /api/v1/users/{id}", "DELETE /api/v1/users/{id}", "GET /api/v1/audit",
	"GET /api/v1/cells/{id}", "PATCH /api/v1/cells/{id}", "POST /api/v1/cells/{id}/revoke",
	"GET /api/v1/enrollment-tokens", "POST /api/v1/enrollment-tokens", "DELETE /api/v1/enrollment-tokens/{id}",
	"GET /api/v1/me/trust", "PUT /api/v1/me/trust",
	"GET /api/v1/organizations", "POST /api/v1/organizations", "GET /api/v1/organizations/{id}", "PATCH /api/v1/organizations/{id}", "DELETE /api/v1/organizations/{id}",
	"DELETE /api/v1/organizations/{id}/membership", "DELETE /api/v1/organizations/{id}/members/{user}", "PATCH /api/v1/organizations/{id}/members/{user}",
	"POST /api/v1/organizations/{id}/invitations", "POST /api/v1/organizations/invitation-preview", "POST /api/v1/organizations/join",
}

func TestEveryAPIRouteHasAScopeDecision(t *testing.T) {
	files, _ := filepath.Glob("api*.go")
	routeRE := regexp.MustCompile(`\bh\("([A-Z]+) "\+apiPrefix\+"(/[^"]*)"`)
	registered := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range routeRE.FindAllStringSubmatch(string(src), -1) {
			registered[m[1]+" "+apiPrefix+m[2]] = true
		}
	}
	if len(registered) < 40 {
		t.Fatalf("found only %d routes: the pattern no longer matches how routes are registered", len(registered))
	}
	only := map[string]bool{}
	for _, r := range sessionOnlyRoutes {
		only[r] = true
		if _, both := apiScopes[r]; both {
			t.Errorf("%s is both session-only and has a scope", r)
		}
		if !registered[r] {
			t.Errorf("%s is listed as session-only but is not a route", r)
		}
	}
	for r := range registered {
		_, scoped := apiScopes[r]
		if !scoped && !only[r] {
			t.Errorf("%s has no decision: give it a scope in apiScopes, or list it as session-only here", r)
		}
	}
	for r, sc := range apiScopes {
		if !registered[r] {
			t.Errorf("apiScopes has %s, which is not a route", r)
		}
		if !apitoken.ValidScope(sc) {
			t.Errorf("%s needs %q, which is not a scope", r, sc)
		}
	}
	// the scopes that exist are all used, and none is for administration
	used := map[string]bool{}
	for _, sc := range apiScopes {
		used[sc] = true
	}
	for _, sc := range apitoken.Scopes {
		if !used[sc.Name] {
			t.Errorf("the scope %s is offered but no route needs it", sc.Name)
		}
	}
}

type tokenEnv struct {
	*env
	handsetID uuid.UUID
	lineID    uuid.UUID
}

func newTokenEnv(t *testing.T) *tokenEnv {
	e := newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.Contacts = &contacts.Store{Pool: s.Pool}
		s.Handsets = &handset.Store{Pool: s.Pool}
		s.SIPAccounts = &sipaccount.Store{Pool: s.Pool, Keyring: kr}
		s.Stats = &stats.Store{Pool: s.Pool}
		s.APITokens = &apitoken.Store{Pool: s.Pool}
	})
	te := &tokenEnv{env: e}
	uid := uidOf(e, "user")
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Home', '301', 'd.example') RETURNING id", uid).Scan(&te.lineID)
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name, model) VALUES ($1, '\\x0a', 10, '\\x', 'Kitchen', 'Gigaset A540') RETURNING id", uid).Scan(&te.handsetID)
	e.store.Pool.Exec(ctx, "INSERT INTO handset_line (handset_id, line, sip_account_id) VALUES ($1, 1, $2)", te.handsetID, te.lineID)
	return te
}

var tokenRE = regexp.MustCompile(`odc_[0-9a-f]{16}_[A-Za-z0-9_-]{43}`)

// makeToken goes through the page, as a person does, and returns the token it shows.
func (e *tokenEnv) makeToken(t *testing.T, browser *http.Client, name, password string, scopes ...string) (string, *http.Response, string) {
	t.Helper()
	form := url.Values{"name": {name}, "password": {password}, "days": {"30"}}
	for _, s := range scopes {
		form.Set("scope_"+s, "on")
	}
	r := e.post(t, browser, "/account/tokens", form)
	body := readBody(r)
	return tokenRE.FindString(body), r, body
}

// bearer calls the API with a token and no session.
func (e *tokenEnv) bearer(t *testing.T, method, path, token, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestMakingAndUsingATokenThroughThePage(t *testing.T) {
	e := newTokenEnv(t)
	u := e.userBrowser(t)
	if resp, body := e.get(t, u, "/account/tokens"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Make a token") || !strings.Contains(body, "No tokens yet") {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	if _, body := e.get(t, u, "/account"); !strings.Contains(body, `href="/account/tokens"`) {
		t.Error("no link from the account page")
	}
	// refused: wrong password, no scope, no name
	if _, r, _ := e.makeToken(t, u, "x", "wrong", "profile:read"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("wrong password: %d", r.StatusCode)
	}
	if tok, r, body := e.makeToken(t, u, "x", "user password"); tok != "" || r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Choose at least one") {
		t.Errorf("no scope: %d %q", r.StatusCode, tok)
	}
	if tok, r, _ := e.makeToken(t, u, "  ", "user password", "profile:read"); tok != "" || r.StatusCode != http.StatusBadRequest {
		t.Errorf("no name: %d", r.StatusCode)
	}
	var n int
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM api_token").Scan(&n)
	if n != 0 {
		t.Fatalf("%d tokens after refusals", n)
	}
	// made: shown once
	tok, r, body := e.makeToken(t, u, "Widget", "user password", "profile:read", "calls:read")
	if tok == "" || r.StatusCode != http.StatusOK || !strings.Contains(body, "only this once") || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("make: %d %q", r.StatusCode, tok)
	}
	for _, p := range []string{"/account/tokens", "/account", "/account/export"} {
		if _, again := e.get(t, u, p); strings.Contains(again, tok) || strings.Contains(again, tok[len(tok)-43:]) {
			t.Errorf("the token is on %s after it was shown", p)
		}
	}
	_, list := e.get(t, u, "/account/tokens")
	if !strings.Contains(list, "Widget") || !strings.Contains(list, "profile:read") || !strings.Contains(list, tok[:20]+"_…") || !strings.Contains(list, ">never<") {
		t.Errorf("list: %s", list)
	}

	// the export of the user's data lists the token by name and scopes, without its secret
	if _, ex := e.get(t, u, "/account/export"); !strings.Contains(ex, `"api_tokens"`) || !strings.Contains(ex, "Widget") || !strings.Contains(ex, "calls:read") {
		t.Errorf("the export does not list the token: %.300s", ex)
	}

	// used: what its scopes allow, and no more
	resp, body := e.bearer(t, "GET", "/api/v1/me", tok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"username":"user"`) {
		t.Fatalf("/me: %d %s", resp.StatusCode, body)
	}
	resp, body = e.bearer(t, "GET", "/api/v1/calls", tok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"items"`) {
		t.Fatalf("/calls: %d %s", resp.StatusCode, body)
	}
	resp, body = e.bearer(t, "GET", "/api/v1/handsets", tok, "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `"insufficient_scope"`) || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `scope="handsets:read"`) {
		t.Errorf("a scope it lacks: %d %s %v", resp.StatusCode, body, resp.Header)
	}
	resp, _ = e.bearer(t, "POST", "/api/v1/calls/1/read", tok, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a write it lacks: %d", resp.StatusCode)
	}
	// the use is on the page
	if _, list = e.get(t, u, "/account/tokens"); strings.Contains(list, ">never<") || !strings.Contains(list, " from ") {
		t.Error("the last use is not shown")
	}
	// the page paths do not take a token, and a token cannot make a token
	resp, _ = e.bearer(t, "GET", "/account", tok, "")
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/login") {
		t.Errorf("a token on a page: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = e.bearer(t, "POST", "/account/tokens", tok, "")
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM api_token").Scan(&n)
	if n != 1 {
		t.Errorf("a token made a token (%d tokens, %d)", n, resp.StatusCode)
	}

	// a choice of lifetime that is not on the page is the default, not a way to a longer token
	var days float64
	if tok3, _, _ := e.makeToken(t, u, "Odd", "user password", "profile:read"); tok3 != "" {
		form := url.Values{"name": {"Long"}, "password": {"user password"}, "days": {"5000"}, "scope_profile:read": {"on"}}
		if r := e.post(t, u, "/account/tokens", form); r.StatusCode != http.StatusOK {
			t.Errorf("a lifetime of 5000 days: %d", r.StatusCode)
		}
		e.store.Pool.QueryRow(ctx, "SELECT extract(epoch FROM expires_at - created_at) / 86400 FROM api_token WHERE name = 'Long'").Scan(&days)
		if days < 89.9 || days > 90.1 {
			t.Errorf("a lifetime of 5000 days gave %.1f days", days)
		}
	}
	// other schemes are not tokens: Basic with a session works as the session, Basic alone is no login, "bearer" in lower case is a token
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/me", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if resp, err := u.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("Basic with a session: %v %v", resp, err)
	}
	if resp, _ = e.bearer(t, "GET", "/api/v1/me", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credentials: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/me", nil)
	req.Header.Set("Authorization", "bearer "+tok)
	if resp, err := (&http.Client{}).Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("a lower-case scheme: %v %v", resp, err)
	}
	// refused: nothing, rubbish, the right shape with the wrong secret
	for _, bad := range []string{"odc_0000000000000000_" + strings.Repeat("A", 43), "rubbish", tok[:len(tok)-1] + "x"} {
		resp, body = e.bearer(t, "GET", "/api/v1/me", bad, "")
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "invalid_token") || strings.Contains(resp.Header.Get("WWW-Authenticate"), "insufficient") {
			t.Errorf("%q: %d %s", bad, resp.StatusCode, body)
		}
	}
	resp, _ = e.bearer(t, "GET", "/api/v1/me", "", "")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("no credentials: %d", resp.StatusCode)
	}
	// a bad token is refused even where the session cookie alone would have been let in
	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer odc_0000000000000000_"+strings.Repeat("A", 43))
	if resp, err := u.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a bad token with a good session: %v %v", resp, err)
	}

	// revoked, expired, a disabled user
	var id uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM api_token").Scan(&id)
	if r := e.post(t, u, "/account/tokens/"+id.String()+"/revoke", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if resp, _ = e.bearer(t, "GET", "/api/v1/me", tok, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked token: %d", resp.StatusCode)
	}
	if _, list = e.get(t, u, "/account/tokens"); !strings.Contains(list, "revoked") {
		t.Error("a revoked token is not shown as revoked")
	}
	tok2, _, _ := e.makeToken(t, u, "Second", "user password", "profile:read")
	if resp, _ = e.bearer(t, "GET", "/api/v1/me", tok2, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("second token: %d", resp.StatusCode)
	}
	e.store.Pool.Exec(ctx, "UPDATE api_token SET expires_at = now() - interval '1 second' WHERE id <> $1", id)
	if resp, _ = e.bearer(t, "GET", "/api/v1/me", tok2, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an expired token: %d", resp.StatusCode)
	}
	e.store.Pool.Exec(ctx, "UPDATE api_token SET expires_at = now() + interval '1 day' WHERE id <> $1", id)
	e.store.Pool.Exec(ctx, "UPDATE app_user SET disabled = true WHERE username = 'user'")
	if resp, _ = e.bearer(t, "GET", "/api/v1/me", tok2, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the token of a disabled user: %d", resp.StatusCode)
	}
	// nobody else can revoke it
	if r := e.post(t, e.adminBrowser(t), "/account/tokens/"+id.String()+"/revoke", url.Values{}); r.StatusCode == http.StatusSeeOther {
		t.Error("another user revoked a token")
	}
}

func TestATokenNeverReachesTheAdministratorsAPI(t *testing.T) {
	e := newTokenEnv(t)
	a := e.adminBrowser(t)
	all := []string{}
	for _, sc := range apitoken.Scopes {
		all = append(all, sc.Name)
	}
	tok, _, _ := e.makeToken(t, a, "Everything", "admin password", all...)
	if tok == "" {
		t.Fatal("no token")
	}
	// the administrator's session can use these; the token, with every scope there is, cannot
	for _, c := range []struct{ method, path string }{{"GET", "/api/v1/users"}, {"GET", "/api/v1/audit"}} {
		if resp, _ := e.get(t, a, c.path); resp.StatusCode != http.StatusOK {
			t.Fatalf("the session on %s: %d", c.path, resp.StatusCode)
		}
		resp, body := e.bearer(t, c.method, c.path, tok, "")
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "token_not_allowed") {
			t.Errorf("a token on %s: %d %s", c.path, resp.StatusCode, body)
		}
	}
	if resp, _ := e.bearer(t, "GET", "/api/v1/me", tok, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("/me with everything: %d", resp.StatusCode)
	}
}

func TestTokenEndpointsForHandsetsCallsAndStatistics(t *testing.T) {
	e := newTokenEnv(t)
	u := e.userBrowser(t)
	uid, other := uidOf(e.env, "user"), uidOf(e.env, "admin")
	var otherLine, otherHandset uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Secret', 's', 'd.example') RETURNING id", other).Scan(&otherLine)
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x0b', 11, '\\x', 'Theirs') RETURNING id", other).Scan(&otherHandset)
	for i, c := range []struct {
		owner     uuid.UUID
		dir       string
		answered  bool
		remote    string
		agoMinute int
	}{{uid, "in", true, "+4930111", 50}, {uid, "in", false, "+4930222", 40}, {uid, "out", true, "0301234", 30}, {other, "in", true, "+49999", 20}} {
		ans := "NULL"
		if c.answered {
			ans = "now() - interval '" + itoa(c.agoMinute) + " minutes' + interval '3 seconds'"
		}
		if _, err := e.store.Pool.Exec(ctx, `INSERT INTO call_log (owner_user_id, direction, remote_party, started_at, answered_at, ended_at, handset_id, sip_account_id)
			VALUES ($1, $2, $3, now() - interval '`+itoa(c.agoMinute)+` minutes', `+ans+`, now() - interval '`+itoa(c.agoMinute-5)+` minutes', $4, $5)`, c.owner, c.dir, c.remote, e.handsetID, e.lineID); err != nil {
			t.Fatal(i, err)
		}
	}
	tok, _, _ := e.makeToken(t, u, "All", "user password", "handsets:read", "handsets:write", "calls:read", "statistics:read", "lines:read", "profile:read")

	// handsets: only the user's own
	resp, body := e.bearer(t, "GET", "/api/v1/handsets", tok, "")
	var list struct {
		Items []apiHandset `json:"items"`
	}
	json.Unmarshal([]byte(body), &list)
	if resp.StatusCode != http.StatusOK || len(list.Items) != 1 || list.Items[0].Name != "Kitchen" || len(list.Items[0].Lines) != 1 || list.Items[0].Lines[0].Label != "Home" || list.Items[0].Model != "Gigaset A540" {
		t.Fatalf("handsets: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "Theirs") || strings.Contains(body, "uak") || strings.Contains(body, "ipui") {
		t.Errorf("the list shows what it should not: %s", body)
	}
	if resp, _ = e.bearer(t, "GET", "/api/v1/handsets/"+e.handsetID.String(), tok, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("get: %d", resp.StatusCode)
	}
	if resp, _ = e.bearer(t, "GET", "/api/v1/handsets/"+otherHandset.String(), tok, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another user's handset: %d", resp.StatusCode)
	}
	if resp, _ = e.bearer(t, "GET", "/api/v1/handsets/not-an-id", tok, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a bad id: %d", resp.StatusCode)
	}
	// patch: a name, a ring policy, the second line; the default line may be cleared with null
	resp, body = e.bearer(t, "PATCH", "/api/v1/handsets/"+e.handsetID.String(), tok, `{"display_name":"Hall","dual_ring_policy":"busy","lines":{"2":"`+e.lineID.String()+`"},"default_line":2}`)
	var got apiHandset
	json.Unmarshal([]byte(body), &got)
	if resp.StatusCode != http.StatusOK || got.Name != "Hall" || got.DualRing != "busy" || len(got.Lines) != 2 || got.DefaultLine == nil || *got.DefaultLine != 2 {
		t.Fatalf("patch: %d %s", resp.StatusCode, body)
	}
	// a patch that does not mention the default line, or the privacy setting, leaves them as they are
	e.store.Pool.Exec(ctx, "UPDATE handset SET disclosed_to_admins = true WHERE id = $1", e.handsetID)
	resp, body = e.bearer(t, "PATCH", "/api/v1/handsets/"+e.handsetID.String(), tok, `{"display_name":"Hall 2"}`)
	json.Unmarshal([]byte(body), &got)
	var disclosed bool
	e.store.Pool.QueryRow(ctx, "SELECT disclosed_to_admins FROM handset WHERE id = $1", e.handsetID).Scan(&disclosed)
	if resp.StatusCode != http.StatusOK || got.Name != "Hall 2" || got.DefaultLine == nil || *got.DefaultLine != 2 || !disclosed {
		t.Errorf("a name-only patch changed more: %d %s (disclosed %v)", resp.StatusCode, body, disclosed)
	}
	resp, body = e.bearer(t, "PATCH", "/api/v1/handsets/"+e.handsetID.String(), tok, `{"default_line":null}`)
	json.Unmarshal([]byte(body), &got)
	if resp.StatusCode != http.StatusOK || got.DefaultLine != nil || got.Name != "Hall 2" {
		t.Errorf("clearing the default line: %d %s", resp.StatusCode, body)
	}
	for name, b := range map[string]string{"bad ring": `{"dual_ring_policy":"loud"}`, "bad line key": `{"lines":{"3":null}}`, "another user's line": `{"lines":{"1":"` + otherLine.String() + `"}}`, "unknown field": `{"colour":"red"}`} {
		if resp, body := e.bearer(t, "PATCH", "/api/v1/handsets/"+e.handsetID.String(), tok, b); resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	if resp, _ = e.bearer(t, "PATCH", "/api/v1/handsets/"+otherHandset.String(), tok, `{"display_name":"Mine now"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("patching another user's handset: %d", resp.StatusCode)
	}
	var name string
	e.store.Pool.QueryRow(ctx, "SELECT display_name FROM handset WHERE id = $1", otherHandset).Scan(&name)
	if name != "Theirs" {
		t.Error("another user's handset was renamed")
	}

	// calls: the user's own, filtered, paged
	var calls struct {
		Items []apiCall `json:"items"`
		Next  *int64    `json:"next"`
	}
	resp, body = e.bearer(t, "GET", "/api/v1/calls", tok, "")
	json.Unmarshal([]byte(body), &calls)
	if resp.StatusCode != http.StatusOK || len(calls.Items) != 3 || calls.Next != nil || strings.Contains(body, "+49999") {
		t.Fatalf("calls: %d %s", resp.StatusCode, body)
	}
	if calls.Items[0].Remote != "0301234" || calls.Items[0].Direction != "out" || calls.Items[0].Result != "answered" || calls.Items[0].DurationSeconds < 290 {
		t.Errorf("the newest call: %+v", calls.Items[0])
	}
	check := func(query string, want int) {
		t.Helper()
		resp, body := e.bearer(t, "GET", "/api/v1/calls"+query, tok, "")
		var c struct{ Items []apiCall }
		json.Unmarshal([]byte(body), &c)
		if resp.StatusCode != http.StatusOK || len(c.Items) != want {
			t.Errorf("%s: %d, %d items: %s", query, resp.StatusCode, len(c.Items), body)
		}
	}
	check("?direction=in", 2)
	check("?missed=true", 1)
	check("?line="+e.lineID.String(), 3)
	check("?line="+otherLine.String(), 0)
	check("?since="+url.QueryEscape(time.Now().Add(-35*time.Minute).UTC().Format(time.RFC3339)), 1)
	check("?limit=2", 2)
	_, body = e.bearer(t, "GET", "/api/v1/calls?limit=2", tok, "")
	json.Unmarshal([]byte(body), &calls)
	if calls.Next == nil {
		t.Fatal("no cursor on a full page")
	}
	check("?limit=2&before="+itoa(int(*calls.Next)), 1)
	for _, q := range []string{"?direction=sideways", "?missed=maybe", "?limit=0", "?limit=501", "?since=yesterday", "?before=x", "?line=x"} {
		if resp, _ := e.bearer(t, "GET", "/api/v1/calls"+q, tok, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", q, resp.StatusCode)
		}
	}
	// one call: the user's; another user's is not found
	var mine, theirs int64
	e.store.Pool.QueryRow(ctx, "SELECT id FROM call_log WHERE owner_user_id = $1 ORDER BY id LIMIT 1", uid).Scan(&mine)
	e.store.Pool.QueryRow(ctx, "SELECT id FROM call_log WHERE owner_user_id = $1", other).Scan(&theirs)
	if resp, _ = e.bearer(t, "GET", "/api/v1/calls/"+itoa(int(mine)), tok, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("a call: %d", resp.StatusCode)
	}
	if resp, _ = e.bearer(t, "GET", "/api/v1/calls/"+itoa(int(theirs)), tok, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another user's call: %d", resp.StatusCode)
	}

	// statistics: the user's own
	resp, body = e.bearer(t, "GET", "/api/v1/statistics/summary?days=3", tok, "")
	var sum struct {
		CallsTotal int `json:"calls_total"`
		Handsets   int `json:"handsets"`
		Days       []struct{ Calls int }
	}
	json.Unmarshal([]byte(body), &sum)
	if resp.StatusCode != http.StatusOK || sum.CallsTotal != 3 || sum.Handsets != 1 || len(sum.Days) != 3 {
		t.Errorf("summary: %d %s", resp.StatusCode, body)
	}
	if resp, _ = e.bearer(t, "GET", "/api/v1/statistics/summary?days=500", tok, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("days=500: %d", resp.StatusCode)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// The scope of an operation in docs/api/openapi.yaml (x-scope) is the one the server enforces; an operation without one is session-only.
func TestSpecScopesMatchTheCode(t *testing.T) {
	b, err := os.ReadFile("../../docs/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromData(b)
	if err != nil {
		t.Fatal(err)
	}
	spec := map[string]string{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			key := method + " " + apiPrefix + pathParamRE.ReplaceAllString(path, "{}")
			sc, _ := op.Extensions["x-scope"].(string)
			spec[key] = sc
		}
	}
	for route, want := range apiScopes {
		key := pathParamRE.ReplaceAllString(route, "{}")
		got, ok := spec[key]
		if !ok {
			t.Errorf("%s is not in the spec", route)
		} else if got != want {
			t.Errorf("%s: the spec says scope %q, the code %q", route, got, want)
		}
	}
	for _, route := range sessionOnlyRoutes {
		if got := spec[pathParamRE.ReplaceAllString(route, "{}")]; got != "" {
			t.Errorf("%s is session-only in the code but the spec gives it the scope %q", route, got)
		}
	}
}
