// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
)

var (
	secretRE    = regexp.MustCompile(`<code class="secret">([A-Z2-7]{32})</code>`)
	challengeRE = regexp.MustCompile(`name="challenge" value="([^"]+)"`)
	recoveryRE  = regexp.MustCompile(`<code class="secret">([a-z2-7]{5}-[a-z2-7]{5})</code>`)
)

func TestTwoFactorLogin(t *testing.T) {
	e := newEnv(t)
	c := e.userBrowser(t)

	// off by default: a plain login works
	_, page := e.get(t, c, "/account/2fa")
	if !strings.Contains(page, "Set up") {
		t.Fatalf("setup page: %s", page)
	}
	// set up: the secret is shown, a wrong code is refused with a new key, the right one turns it on
	r := e.post(t, c, "/account/2fa/start", nil)
	body := readBody(r)
	m := secretRE.FindStringSubmatch(body)
	if r.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("start: %d %s", r.StatusCode, body)
	}
	if r := e.post(t, c, "/account/2fa/confirm", url.Values{"code": {"000000"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a wrong code was accepted: %d", r.StatusCode)
	}
	// the refusal made a new key
	r = e.post(t, c, "/account/2fa/start", nil)
	m = secretRE.FindStringSubmatch(readBody(r))
	code, err := auth.TOTPCode(m[1], time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r = e.post(t, c, "/account/2fa/confirm", url.Values{"code": {code}})
	body = readBody(r)
	codes := recoveryRE.FindAllStringSubmatch(body, -1)
	if r.StatusCode != http.StatusOK || len(codes) != auth.RecoveryCodes {
		t.Fatalf("confirm: %d, %d recovery codes", r.StatusCode, len(codes))
	}
	if _, page := e.get(t, c, "/account/2fa"); strings.Contains(page, codes[0][1]) || !strings.Contains(page, "10 unused") {
		t.Error("the recovery codes are shown again, or the count is wrong")
	}

	// now a login needs the second step
	c2 := e.browser()
	r = e.login(t, c2, "user", "user password")
	body = readBody(r)
	ch := challengeRE.FindStringSubmatch(body)
	if r.StatusCode != http.StatusOK || ch == nil || len(r.Cookies()) != 0 {
		t.Fatalf("after the password: %d, cookies %v", r.StatusCode, r.Cookies())
	}
	if resp, _ := e.get(t, c2, "/account"); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("logged in after the password alone")
	}
	if r := e.post(t, c2, "/login/2fa", url.Values{"challenge": {ch[1]}, "code": {"123456"}}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", r.StatusCode)
	}
	if r := e.post(t, c2, "/login/2fa", url.Values{"challenge": {"nonsense"}, "code": {code}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad challenge: %d", r.StatusCode)
	}
	// the code just used at setup cannot be used again (replay), the next step's can
	if r := e.post(t, c2, "/login/2fa", url.Values{"challenge": {ch[1]}, "code": {code}}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a used code was accepted again: %d", r.StatusCode)
	}
	next, _ := auth.TOTPCode(m[1], time.Now().Add(auth.TOTPPeriod*time.Second))
	if r := e.post(t, c2, "/login/2fa", url.Values{"challenge": {ch[1]}, "code": {next}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("the next code: %d", r.StatusCode)
	}
	if resp, _ := e.get(t, c2, "/account"); resp.StatusCode != http.StatusOK {
		t.Fatalf("not logged in after both steps: %d", resp.StatusCode)
	}

	// a recovery code works once
	c3 := e.browser()
	ch = challengeRE.FindStringSubmatch(readBody(e.login(t, c3, "user", "user password")))
	if r := e.post(t, c3, "/login/2fa", url.Values{"challenge": {ch[1]}, "code": {codes[0][1]}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("recovery code: %d", r.StatusCode)
	}
	c4 := e.browser()
	ch = challengeRE.FindStringSubmatch(readBody(e.login(t, c4, "user", "user password")))
	if r := e.post(t, c4, "/login/2fa", url.Values{"challenge": {ch[1]}, "code": {codes[0][1]}}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a recovery code worked twice: %d", r.StatusCode)
	}

	// turning it off needs the password
	if r := e.post(t, c, "/account/2fa/disable", url.Values{"password": {"wrong"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("disable with a wrong password: %d", r.StatusCode)
	}
	if r := e.post(t, c, "/account/2fa/disable", url.Values{"password": {"user password"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("disable: %d", r.StatusCode)
	}
	if r := e.login(t, e.browser(), "user", "user password"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("login after turning it off: %d", r.StatusCode)
	}
}

func TestTwoFactorWrongCodesAreLimited(t *testing.T) {
	e := newEnv(t)
	c := e.userBrowser(t)
	r := e.post(t, c, "/account/2fa/start", nil)
	m := secretRE.FindStringSubmatch(readBody(r))
	code, _ := auth.TOTPCode(m[1], time.Now())
	e.post(t, c, "/account/2fa/confirm", url.Values{"code": {code}})

	c2 := e.browser()
	ch := challengeRE.FindStringSubmatch(readBody(e.login(t, c2, "user", "user password")))
	var last int
	for range 8 {
		last = e.post(t, c2, "/login/2fa", url.Values{"challenge": {ch[1]}, "code": {"000000"}}).StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("guessing codes is not limited: last status %d", last)
	}
}

func TestAdminsNeedTwoFactorWhenRequired(t *testing.T) {
	e := newEnv(t)
	// the test environment turns the requirement off; this test turns it on
	if err := e.app.Settings.Put(ctx, settings.KeySecurity, settings.Security{RequireAdmin2FA: true}); err != nil {
		t.Fatal(err)
	}
	a, u := e.adminBrowser(t), e.userBrowser(t)
	for _, p := range []string{"/admin", "/admin/users", "/admin/settings", "/admin/audit"} {
		resp, _ := e.get(t, a, p)
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/account/2fa") {
			t.Errorf("%s for an admin without two-factor: %d %s", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// the way out stays open, and says why
	resp, page := e.get(t, a, "/account/2fa?need=1")
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Administrators need two-factor login") {
		t.Fatalf("setup page: %d", resp.StatusCode)
	}
	for _, p := range []string{"/", "/account", "/handsets"} {
		if resp, _ := e.get(t, a, p); resp.StatusCode != http.StatusOK {
			t.Errorf("%s was shut for the admin too: %d", p, resp.StatusCode)
		}
	}
	// every page tells the admin why the administration pages are missing, instead of the optional offer
	if _, page := e.get(t, a, "/handsets"); !strings.Contains(page, "administration pages stay hidden") || strings.Contains(page, "Protect your account") {
		t.Error("an admin without two-factor is not told why the admin pages are hidden")
	}
	// the admin API is no way round it
	if resp, _ := e.get(t, a, "/api/v1/users"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("/api/v1/users for an admin without two-factor: %d", resp.StatusCode)
	}
	// users are not affected
	if resp, _ := e.get(t, u, "/statistics"); resp.StatusCode != http.StatusOK {
		t.Errorf("a user's page: %d", resp.StatusCode)
	}
	// set it up: the admin pages open
	m := secretRE.FindStringSubmatch(readBody(e.post(t, a, "/account/2fa/start", nil)))
	code, _ := auth.TOTPCode(m[1], time.Now())
	if r := e.post(t, a, "/account/2fa/confirm", url.Values{"code": {code}}); r.StatusCode != http.StatusOK {
		t.Fatalf("setup: %d", r.StatusCode)
	}
	if resp, _ := e.get(t, a, "/admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("/admin after setup: %d", resp.StatusCode)
	}
	if resp, _ := e.get(t, a, "/api/v1/users"); resp.StatusCode != http.StatusOK {
		t.Errorf("/api/v1/users after setup: %d", resp.StatusCode)
	}
	// switched off in the settings, the requirement is gone for the next admin
	if err := e.app.Settings.Put(ctx, settings.KeySecurity, settings.Security{RequireAdmin2FA: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Pool.Exec(ctx, "DELETE FROM user_totp"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.get(t, a, "/admin"); resp.StatusCode != http.StatusOK {
		t.Errorf("/admin with the requirement off: %d", resp.StatusCode)
	}
	// and the switch itself is the admin's
	if r := e.post(t, u, "/admin/settings/security", url.Values{"require_admin_2fa": {"on"}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("a user set the requirement: %d", r.StatusCode)
	}
}
