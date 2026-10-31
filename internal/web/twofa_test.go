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
	for i := 0; i < 8; i++ {
		last = e.post(t, c2, "/login/2fa", url.Values{"challenge": {ch[1]}, "code": {"000000"}}).StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("guessing codes is not limited: last status %d", last)
	}
}
