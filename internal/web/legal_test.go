// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/legal"
)

func legalEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, func(s *Server) { s.Legal = &legal.Store{Pool: s.Pool, Settings: s.Settings} })
}

func TestLegalTextsGateEveryoneUntilTheySignOff(t *testing.T) {
	e := legalEnv(t)
	anon, user, admin := e.browser(), e.browser(), e.browser()
	e.login(t, user, "user", "user password")
	e.login(t, admin, "admin", "admin password")

	// nothing switched on: no page, no menu entry, no gate
	if resp, _ := e.get(t, anon, "/legal"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/legal with nothing on: %d", resp.StatusCode)
	}
	if _, body := e.get(t, user, "/handsets"); strings.Contains(body, `href="/legal"`) {
		t.Error("the menu links to a legal notice that does not exist")
	}

	// the administrator switches texts on
	form := url.Values{"enabled_imprint": {"on"}, "text_imprint": {"Example e.V.\n\nExamplestreet 1"}, "enabled_experimental": {"on"}, "text_experimental": {"Mondays are maintenance days."}}
	if r := e.post(t, admin, "/admin/legal", form); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin save: %d", r.StatusCode)
	}
	// the administrator is asked like everybody else, so there is no way to switch a text on and stay outside it
	if r, _ := e.get(t, admin, "/admin/legal"); r.StatusCode != http.StatusSeeOther {
		t.Errorf("the admin who switched a text on is not asked to sign off: %d", r.StatusCode)
	}
	if resp, body := e.get(t, anon, "/legal"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Examplestreet 1") ||
		!strings.Contains(body, "Mondays are maintenance days.") || !strings.Contains(body, "legal-scroll") {
		t.Fatalf("the legal notice for everyone: %d %q", resp.StatusCode, body)
	}

	// the user (and the admin) are stopped everywhere but the sign-off page
	for _, c := range []*http.Client{user, admin} {
		resp, _ := e.get(t, c, "/handsets")
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/legal/accept" {
			t.Fatalf("before the sign-off: %d %q", resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if resp, _ := e.get(t, admin, "/admin/settings"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the admin pages are open to an admin who has not signed off: %d", resp.StatusCode)
	}
	resp, body := e.get(t, user, "/legal/accept")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="accept_imprint"`) || !strings.Contains(body, `name="accept_experimental"`) {
		t.Fatalf("sign-off page: %d %q", resp.StatusCode, body)
	}
	// one box is not enough
	one := url.Values{"accept_imprint": {versionIn(t, body, "imprint")}}
	if r := e.post(t, user, "/legal/accept", one); r.StatusCode != http.StatusBadRequest {
		t.Errorf("one box: %d", r.StatusCode)
	}
	if r, _ := e.get(t, user, "/handsets"); r.StatusCode != http.StatusSeeOther {
		t.Error("one box opened the door")
	}
	all := url.Values{"accept_imprint": {versionIn(t, body, "imprint")}, "accept_experimental": {versionIn(t, body, "experimental")}}
	if r := e.post(t, user, "/legal/accept", all); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-off: %d", r.StatusCode)
	}
	if r, b := e.get(t, user, "/handsets"); r.StatusCode != http.StatusOK || !strings.Contains(b, `href="/legal"`) {
		t.Errorf("after the sign-off: %d, the menu has no Legal notice entry", r.StatusCode)
	}

	// the API is closed to someone who has not signed off, too
	if resp, body := e.get(t, admin, "/api/v1/me"); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "legal_acceptance_required") {
		t.Errorf("the API before the sign-off: %d %q", resp.StatusCode, body)
	}

	// the administrator signs off; a text that needs words is refused empty, and the form comes back as it was sent
	signOff(t, e, admin)
	resp, body = e.get(t, admin, "/admin/legal")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "legal-scroll") {
		t.Fatalf("admin page: %d", resp.StatusCode)
	}
	if r := e.post(t, admin, "/admin/legal", url.Values{"enabled_privacy": {"on"}, "text_imprint": {"kept in the form"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an empty privacy notice: %d", r.StatusCode)
	}
	if resp, body := e.get(t, admin, "/api/v1/me"); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"username"`) {
		t.Errorf("the API after the sign-off: %d %q", resp.StatusCode, body)
	}

	// changing a text asks the user again
	form["text_imprint"] = []string{"Example e.V., a new address"}
	if r := e.post(t, admin, "/admin/legal", form); r.StatusCode != http.StatusSeeOther {
		t.Fatal("admin save 2")
	}
	if r, _ := e.get(t, user, "/handsets"); r.StatusCode != http.StatusSeeOther {
		t.Errorf("a changed text did not ask again: %d", r.StatusCode)
	}
}

var signBox = regexp.MustCompile(`name="accept_([a-z]+)" value="([0-9]+)"`)

// signOff ticks every box of the sign-off page for the client's user.
func signOff(t *testing.T, e *env, c *http.Client) {
	t.Helper()
	_, body := e.get(t, c, "/legal/accept")
	form := url.Values{}
	for _, m := range signBox.FindAllStringSubmatch(body, -1) {
		form.Set("accept_"+m[1], m[2])
	}
	if len(form) == 0 {
		t.Fatalf("nothing to sign off: %q", body)
	}
	if r := e.post(t, c, "/legal/accept", form); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-off: %d", r.StatusCode)
	}
}

// versionIn reads the version a sign-off box carries.
func versionIn(t *testing.T, body, kind string) string {
	t.Helper()
	marker := `name="accept_` + kind + `" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no box for %s in %q", kind, body)
	}
	rest := body[i+len(marker):]
	return rest[:strings.Index(rest, `"`)]
}

func TestRegistrationNeedsEveryBoxTicked(t *testing.T) {
	e := legalEnv(t)
	admin := e.browser()
	e.login(t, admin, "admin", "admin password")
	if r := e.post(t, admin, "/admin/settings/registration", url.Values{"enabled": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatal("registration on")
	}
	if r := e.post(t, admin, "/admin/legal", url.Values{"enabled_emergency": {"on"}, "enabled_experimental": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatal("legal on")
	}
	c := e.browser()
	resp, body := e.get(t, c, "/register")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `name="accept_emergency"`) || !strings.Contains(body, `name="accept_experimental"`) {
		t.Fatalf("register page: %d %q", resp.StatusCode, body)
	}
	person := url.Values{"username": {"neo"}, "email": {"neo@example.org"}, "password": {"long enough password"}}
	if r := e.post(t, c, "/register", person); r.StatusCode != http.StatusBadRequest {
		t.Errorf("no box ticked: %d", r.StatusCode)
	}
	person.Set("accept_emergency", versionIn(t, body, "emergency"))
	if r := e.post(t, c, "/register", person); r.StatusCode != http.StatusBadRequest {
		t.Errorf("one of two boxes: %d", r.StatusCode)
	}
	person.Set("accept_experimental", versionIn(t, body, "experimental"))
	r := e.post(t, c, "/register", person)
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("both boxes: %d", r.StatusCode)
	}
	// signed in at once, and with nothing left to sign
	if r, _ := e.get(t, c, "/handsets"); r.StatusCode != http.StatusOK {
		t.Errorf("after registering: %d", r.StatusCode)
	}
}
