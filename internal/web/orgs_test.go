// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/org"
)

var (
	orgLocRE   = regexp.MustCompile(`^/organizations/([0-9a-f-]{36})`)
	orgTokenRE = regexp.MustCompile(`organizations/join\?token=([A-Za-z0-9_%-]+)`)
)

func TestOrganizationPages(t *testing.T) {
	e := newEnvWith(t, func(s *Server) { s.Orgs = &org.Store{Pool: s.Pool} })
	for _, name := range []string{"carol", "dave"} {
		if _, err := e.store.CreateLocalUser(ctx, auth.NewLocalUser{Username: name, Password: name + " password", Role: auth.RoleUser}); err != nil {
			t.Fatal(err)
		}
	}
	owner, carol, dave, sys := e.browser(), e.browser(), e.browser(), e.browser()
	e.login(t, owner, "user", "user password")
	e.login(t, carol, "carol", "carol password")
	e.login(t, dave, "dave", "dave password")
	e.login(t, sys, "admin", "admin password")

	// The page that offers the choice tells what a base station's operator can read, and that DECT is the reason.
	resp, body := e.get(t, owner, "/organizations")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	for _, want := range []string{"can read your handset", "This is how DECT is", "cannot change it", "registered again", "Whose base stations may my handsets use?"} {
		if !strings.Contains(html.UnescapeString(body), want) {
			t.Errorf("the trust page does not say %q", want)
		}
	}
	if resp, body := e.get(t, owner, "/handsets/register"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "can read your handset") {
		t.Errorf("the registration page has no disclosure: %d", resp.StatusCode)
	}

	// Create: the creator is the owner.
	r := e.post(t, owner, "/organizations", url.Values{"name": {"Makerspace"}})
	m := orgLocRE.FindStringSubmatch(r.Header.Get("Location"))
	if r.StatusCode != http.StatusSeeOther || m == nil {
		t.Fatalf("create: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	id := m[1]
	if r := e.post(t, owner, "/organizations", url.Values{"name": {" "}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an empty name: %d", r.StatusCode)
	}
	_, page := e.get(t, owner, "/organizations/"+id)
	for _, want := range []string{"Makerspace", "Base stations", "Handsets", "0 online · 0 offline", "Invite someone", "Delete"} {
		if !strings.Contains(page, want) {
			t.Errorf("owner's page lacks %q", want)
		}
	}

	// A stranger is told nothing: not even that it exists.
	if resp, _ := e.get(t, dave, "/organizations/"+id); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a non-member saw the organization: %d", resp.StatusCode)
	}
	if r := e.post(t, dave, "/organizations/"+id+"/invite", url.Values{}); r.StatusCode != http.StatusNotFound {
		t.Errorf("a non-member invited: %d", r.StatusCode)
	}

	// The owner invites; the token is on the page once.
	r = e.post(t, owner, "/organizations/"+id+"/invite", url.Values{})
	inv := readBody(r)
	tm := orgTokenRE.FindStringSubmatch(html.UnescapeString(inv))
	if r.StatusCode != http.StatusOK || tm == nil || !strings.Contains(inv, "shown only now") {
		t.Fatalf("invite: %d %q", r.StatusCode, inv)
	}
	token, _ := url.QueryUnescape(tm[1])
	if _, again := e.get(t, owner, "/organizations/"+id); strings.Contains(again, token) {
		t.Error("the token is shown again after the invitation")
	}

	// Joining: the owner's name is suggested, the member may choose another.
	resp, form := e.get(t, carol, "/organizations/join?token="+url.QueryEscape(token))
	if resp.StatusCode != http.StatusOK || !strings.Contains(form, `value="Makerspace"`) {
		t.Fatalf("join form: %d (no suggestion)", resp.StatusCode)
	}
	if resp, _ := e.get(t, carol, "/organizations/join?token=not-a-real-token-0123456789"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a made-up token: %d", resp.StatusCode)
	}
	r = e.post(t, carol, "/organizations/join", url.Values{"token": {token}, "confirm": {"1"}, "alias": {"Our crew"}})
	if r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), "/organizations/"+id) {
		t.Fatalf("join: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := e.post(t, dave, "/organizations/join", url.Values{"token": {token}, "confirm": {"1"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a used token worked: %d", r.StatusCode)
	}
	_, cpage := e.get(t, carol, "/organizations/"+id)
	for _, want := range []string{"Our crew", "owner", "carol", "Your name for it"} {
		if !strings.Contains(cpage, want) {
			t.Errorf("member's page lacks %q", want)
		}
	}
	for _, not := range []string{"Invite someone", "Rename the organization", `action="/organizations/` + id + `/delete"`} {
		if strings.Contains(cpage, not) {
			t.Errorf("a plain member sees %q", not)
		}
	}
	if r := e.post(t, carol, "/organizations/"+id+"/invite", url.Values{}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a member invited: %d", r.StatusCode)
	}
	if r := e.post(t, carol, "/organizations/"+id+"/delete", url.Values{"confirm": {"Makerspace"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a member deleted the organization: %d", r.StatusCode)
	}

	// The trust setting.
	if r := e.post(t, carol, "/organizations/trust", url.Values{"scope": {"everybody"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown setting: %d", r.StatusCode)
	}
	if r := e.post(t, carol, "/organizations/trust", url.Values{"scope": {"orgs"}, "org": {id}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("save trust: %d", r.StatusCode)
	}
	_, list := e.get(t, carol, "/organizations")
	if !regexp.MustCompile(`value="orgs"\s+checked`).MatchString(list) || !strings.Contains(list, `value="`+id+`" checked`) {
		t.Error("the saved trust setting is not shown")
	}

	// Admins of the organization remove members; owner appoints.
	_, opage := e.get(t, owner, "/organizations/"+id)
	um := regexp.MustCompile(`/organizations/`+id+`/members/([0-9a-f-]{36})/remove`).FindAllStringSubmatch(opage, -1)
	if len(um) != 1 { // the owner has no Remove button, only carol
		t.Fatalf("remove buttons: %d", len(um))
	}
	carolID := um[0][1]
	if r := e.post(t, owner, "/organizations/"+id+"/members/"+carolID+"/role", url.Values{"role": {"admin"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("appoint: %d", r.StatusCode)
	}
	if _, cp := e.get(t, carol, "/organizations/"+id); !strings.Contains(cp, "Invite someone") {
		t.Error("an appointed admin cannot invite")
	}
	if r := e.post(t, owner, "/organizations/"+id+"/members/"+carolID+"/remove", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove: %d", r.StatusCode)
	}
	if resp, _ := e.get(t, carol, "/organizations/"+id); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a removed member still sees the organization: %d", resp.StatusCode)
	}
	// The trust in it ended with the membership.
	if _, l := e.get(t, carol, "/organizations"); strings.Contains(l, `value="`+id+`"`) {
		t.Error("a removed member still has the organization to trust")
	}

	// The controller admin sees every organization and can open one they are not in.
	if _, al := e.get(t, sys, "/organizations"); !strings.Contains(al, "All organizations") || !strings.Contains(al, "Makerspace") {
		t.Error("the administrator's list of all organizations")
	}
	if resp, _ := e.get(t, sys, "/organizations/"+id); resp.StatusCode != http.StatusOK {
		t.Errorf("the administrator could not open it: %d", resp.StatusCode)
	}
	// Delete asks for the name.
	if r := e.post(t, owner, "/organizations/"+id+"/delete", url.Values{"confirm": {"wrong"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("deleted without the name: %d", r.StatusCode)
	}
	if r := e.post(t, owner, "/organizations/"+id+"/delete", url.Values{"confirm": {"Makerspace"}}); r.StatusCode != http.StatusSeeOther {
		t.Errorf("delete: %d", r.StatusCode)
	}
}

// A trust setting that leaves a user's handsets with no base station asks first; and a user in that
// state sees the warning where they look at their handsets.
func TestTrustThatStrandsHandsetsAsksFirst(t *testing.T) {
	e := newEnvWith(t, func(s *Server) { s.Orgs = &org.Store{Pool: s.Pool} })
	c := e.browser()
	e.login(t, c, "user", "user password")
	var uid string
	if err := e.app.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Pool.Exec(ctx, `INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\x0102030405', 1, '\x00')`, uid); err != nil {
		t.Fatal(err)
	}
	// Nobody has a base station here: 'all' is fine (there is nothing to reach, and nothing was taken away), 'own' is not.
	r := e.post(t, c, "/organizations/trust", url.Values{"scope": {"own"}})
	body := readBody(r)
	if r.StatusCode != http.StatusConflict || !strings.Contains(body, "without a base station") || !strings.Contains(body, "Nothing has been saved yet") ||
		!strings.Contains(body, `name="confirm" value="1"`) {
		t.Fatalf("the question: %d", r.StatusCode)
	}
	if tr, _ := e.app.Orgs.GetTrust(ctx, &auth.User{ID: uuid.MustParse(uid)}); tr.Scope != org.ScopeAll {
		t.Fatal("the setting was saved before the user confirmed")
	}
	// Confirmed, it is saved, and the pages warn.
	if r := e.post(t, c, "/organizations/trust", url.Values{"scope": {"own"}, "confirm": {"1"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirmed: %d", r.StatusCode)
	}
	for _, path := range []string{"/organizations", "/handsets"} {
		if _, b := e.get(t, c, path); !strings.Contains(b, "None of your handset can use a base station") {
			t.Errorf("%s has no warning", path)
		}
	}
	// The widest setting is saved without a question, and with no base station anywhere the warning says that.
	if r := e.post(t, c, "/organizations/trust", url.Values{"scope": {"all"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("back to all: %d", r.StatusCode)
	}
	if _, b := e.get(t, c, "/handsets"); !strings.Contains(b, "has no active base station at the moment") {
		t.Error("no warning that the controller has no base station")
	}
}
