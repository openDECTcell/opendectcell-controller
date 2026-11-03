// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
)

// enable2FA turns two-factor login on for the user c is logged in as and returns the recovery codes.
func enable2FA(t *testing.T, e *env, c *http.Client) (secret string, codes []string) {
	t.Helper()
	r := e.post(t, c, "/account/2fa/start", nil)
	body := readBody(r)
	m := secretRE.FindStringSubmatch(body)
	if r.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("start: %d", r.StatusCode)
	}
	if !strings.Contains(body, `<div class="qr`) || !strings.Contains(body, "<svg") || !strings.Contains(body, "<path") {
		t.Error("the setup page shows no QR code next to the key")
	}
	if !strings.Contains(body, `href="otpauth://totp/`) {
		t.Error("the link to open the key in an app is dead")
	}
	code, _ := auth.TOTPCode(m[1], time.Now())
	r = e.post(t, c, "/account/2fa/confirm", url.Values{"code": {code}})
	for _, cc := range recoveryRE.FindAllStringSubmatch(readBody(r), -1) {
		codes = append(codes, cc[1])
	}
	if r.StatusCode != http.StatusOK || len(codes) != auth.RecoveryCodes {
		t.Fatalf("confirm: %d, %d codes", r.StatusCode, len(codes))
	}
	return m[1], codes
}

// loginAsksCode logs user in with the password and says whether the second step came up.
func loginAsksCode(t *testing.T, e *env, c *http.Client) (challenge string, asked bool) {
	t.Helper()
	r := e.login(t, c, "user", "user password")
	if r.StatusCode == http.StatusSeeOther {
		return "", false
	}
	ch := challengeRE.FindStringSubmatch(readBody(r))
	if r.StatusCode != http.StatusOK || ch == nil {
		t.Fatalf("login: %d", r.StatusCode)
	}
	return ch[1], true
}

// Two-factor login asks on a device it has not seen and on a known one every three weeks.
func TestTrustedDevices(t *testing.T) {
	e := newEnv(t)
	c := e.userBrowser(t)
	_, codes := enable2FA(t, e, c)
	used := 0
	nextCode := func() string { used++; return codes[used-1] }

	// the browser that set it up is known: a new login there asks for the password only
	e.post(t, c, "/logout", nil)
	if _, asked := loginAsksCode(t, e, c); asked {
		t.Fatal("the browser that set two-factor login up was asked again at once")
	}

	// another browser is new: it is asked, and if it says so, it is known afterwards
	c2 := e.browser()
	ch, asked := loginAsksCode(t, e, c2)
	if !asked {
		t.Fatal("a new device was not asked for a code")
	}
	if r := e.post(t, c2, "/login/2fa", url.Values{"challenge": {ch}, "code": {nextCode()}, "remember": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("second step: %d", r.StatusCode)
	}
	e.post(t, c2, "/logout", nil)
	if _, asked := loginAsksCode(t, e, c2); asked {
		t.Error("a device that passed the second step was asked again within three weeks")
	}

	// a third one that does not tick the box is asked every time
	c3 := e.browser()
	ch, _ = loginAsksCode(t, e, c3)
	if r := e.post(t, c3, "/login/2fa", url.Values{"challenge": {ch}, "code": {nextCode()}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("second step without the box: %d", r.StatusCode)
	}
	e.post(t, c3, "/logout", nil)
	if _, asked := loginAsksCode(t, e, c3); !asked {
		t.Error("a device that was not to be remembered was not asked")
	}

	// after three weeks a known device is asked again, and is known for three more after that
	e.store.Pool.Exec(ctx, "UPDATE trusted_device SET last_2fa_at = now() - interval '22 days'")
	e.post(t, c2, "/logout", nil)
	ch, asked = loginAsksCode(t, e, c2)
	if !asked {
		t.Fatal("a device not asked for 22 days was not asked")
	}
	if r := e.post(t, c2, "/login/2fa", url.Values{"challenge": {ch}, "code": {nextCode()}, "remember": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("second step after three weeks: %d", r.StatusCode)
	}
	e.post(t, c2, "/logout", nil)
	if _, asked := loginAsksCode(t, e, c2); asked {
		t.Error("the three weeks did not start again after a code")
	}
	e.post(t, c2, "/logout", nil)
	e.store.Pool.Exec(ctx, "UPDATE trusted_device SET last_2fa_at = now() - interval '20 days'")
	if _, asked := loginAsksCode(t, e, c2); asked {
		t.Error("a device asked 20 days ago was asked again")
	}

	// a garbage cookie is no device
	c4 := e.browser()
	u, _ := url.Parse(e.srv.URL)
	c4.Jar.SetCookies(u, []*http.Cookie{{Name: "opendectcell_device", Value: "x' OR 'a'='a"}})
	if _, asked := loginAsksCode(t, e, c4); !asked {
		t.Error("a made-up device cookie passed")
	}

	// "ask on every device again" and a new password forget them all; so does turning it off
	if _, page := e.get(t, e.loggedIn(t, c2), "/account/2fa"); !strings.Contains(page, "Ask on every device again") {
		t.Error("no way to forget the devices")
	}
	e.post(t, c2, "/account/2fa/devices/forget", nil)
	e.post(t, c2, "/logout", nil)
	if _, asked := loginAsksCode(t, e, c2); !asked {
		t.Error("a forgotten device was not asked")
	}
	var n int
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM trusted_device").Scan(&n)
	if n != 0 {
		t.Errorf("%d devices left after forgetting them", n)
	}
}

// loggedIn returns c if it is logged in (it is the one a test just used for a login that succeeded).
func (e *env) loggedIn(t *testing.T, c *http.Client) *http.Client {
	t.Helper()
	if r, _ := e.get(t, c, "/account"); r.StatusCode != http.StatusOK {
		t.Fatalf("not logged in: %d", r.StatusCode)
	}
	return c
}

// Two-factor login is offered, not forced: a banner on every page that "not now" puts away for a month.
func TestTwoFactorOfferIsNotAnOrder(t *testing.T) {
	e := newEnv(t)
	c := e.userBrowser(t)
	const banner = "Protect your account with two-factor login"
	if _, page := e.get(t, c, "/account"); !strings.Contains(page, banner) {
		t.Fatal("a user without two-factor login is not offered it")
	}
	if r, _ := e.get(t, c, "/handsets"); r.StatusCode == http.StatusSeeOther || r.StatusCode == http.StatusForbidden {
		t.Errorf("not having two-factor login blocks a user: %d", r.StatusCode)
	}
	if _, page := e.get(t, c, "/account/2fa"); strings.Contains(page, banner) {
		t.Error("the offer is shown on the page that does the setting up")
	}
	if r := e.post(t, c, "/account/2fa/later", url.Values{"next": {"/account"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("not now: %d", r.StatusCode)
	}
	if _, page := e.get(t, c, "/account"); strings.Contains(page, banner) {
		t.Error("the offer came back at once after 'not now'")
	}
	e.store.Pool.Exec(ctx, "UPDATE app_user SET twofa_prompt_dismissed_at = now() - interval '31 days'")
	if _, page := e.get(t, c, "/account"); !strings.Contains(page, banner) {
		t.Error("the offer did not come back after a month")
	}
	enable2FA(t, e, c)
	if _, page := e.get(t, c, "/account"); strings.Contains(page, banner) {
		t.Error("a user with two-factor login is offered it")
	}
}

// An administrator the settings require to have two-factor login, and who has none, is a plain user everywhere,
// not only on the admin pages (review finding W4).
func TestAdminWithoutTwoFactorHasNoAdminPowers(t *testing.T) {
	e := newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Cells, err = cell.Open(ctx, s.Pool, kr, "dect.example.org:7300"); err != nil {
			t.Fatal(err)
		}
		s.CellLink = &fakeLink{states: map[uuid.UUID]wire.CellStateValue{}}
	})
	var uid, hall uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state, enrolled_by) VALUES ('Hall', 'active', $1) RETURNING id", uid).Scan(&hall)
	a := e.adminBrowser(t)

	if r, _ := e.get(t, a, "/stations/"+hall.String()); r.StatusCode != http.StatusOK {
		t.Fatalf("an admin on a station page with no requirement: %d", r.StatusCode)
	}
	if err := e.app.Settings.Put(ctx, settings.KeySecurity, settings.Security{RequireAdmin2FA: true}); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.get(t, a, "/stations/"+hall.String()); r.StatusCode != http.StatusNotFound {
		t.Errorf("an admin without two-factor on another user's station page: %d", r.StatusCode)
	}
	if r, _ := e.get(t, a, "/admin/users"); r.StatusCode != http.StatusSeeOther {
		t.Errorf("an admin without two-factor on an admin page: %d", r.StatusCode)
	}
	if r, _ := e.get(t, a, "/account"); r.StatusCode != http.StatusOK {
		t.Errorf("an admin without two-factor on their own account page: %d", r.StatusCode)
	}
	enable2FA(t, e, a)
	if r, _ := e.get(t, a, "/stations/"+hall.String()); r.StatusCode != http.StatusOK {
		t.Errorf("an admin with two-factor on a station page: %d", r.StatusCode)
	}
}
