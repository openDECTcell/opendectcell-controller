// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestDeviceLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0":                                                                  "Firefox on Linux",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36 Edg/130.0":                   "Edge on Windows",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1": "Safari on iOS",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36":                       "Chrome on Android",
		"curl/8.5.0": "",
		"":           "",
	} {
		if got := deviceLabel(ua); got != want {
			t.Errorf("deviceLabel(%q) = %q, want %q", ua, got, want)
		}
	}
}

var sessionIDRE = regexp.MustCompile(`name="id" value="([0-9a-f]{16})"`)

// A user sees their sessions, ends one, and ends all the others; nobody ends another user's session; the session that asks
// is never ended by it.
func TestSessionsList(t *testing.T) {
	e := newEnv(t)
	login := func() *http.Client {
		c := e.browser()
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/login", strings.NewReader(url.Values{"username": {"user"}, "password": {"user password"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0")
		resp, err := c.Do(req)
		if err != nil || resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("login: %v %v", resp, err)
		}
		return c
	}
	a, b, c := login(), login(), login()
	other := e.adminBrowser(t)

	resp, page := e.get(t, a, "/account/sessions")
	if resp.StatusCode != http.StatusOK || strings.Count(page, "Firefox on Linux") != 3 || strings.Count(page, "this one") != 1 {
		t.Fatalf("the list: %d, %d browsers, %d marked\n%s", resp.StatusCode, strings.Count(page, "Firefox on Linux"), strings.Count(page, "this one"), page)
	}
	ids := sessionIDRE.FindAllStringSubmatch(page, -1)
	if len(ids) != 2 {
		t.Fatalf("%d sessions with a button, want 2 (the current one has none)", len(ids))
	}

	// another user cannot end it with the ID
	if r := e.post(t, other, "/account/sessions/end", url.Values{"id": {ids[0][1]}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin's try: %d", r.StatusCode)
	}
	for name, x := range map[string]*http.Client{"b": b, "c": c} {
		if resp, _ := e.get(t, x, "/account"); resp.StatusCode != http.StatusOK {
			t.Fatalf("another user's request ended session %s", name)
		}
	}
	// the user ends one of the others
	if r := e.post(t, a, "/account/sessions/end", url.Values{"id": {ids[0][1]}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("end: %d", r.StatusCode)
	}
	alive := 0
	for _, x := range []*http.Client{b, c} {
		if resp, _ := e.get(t, x, "/account"); resp.StatusCode == http.StatusOK {
			alive++
		}
	}
	if alive != 1 {
		t.Fatalf("%d of the two other sessions are alive after ending one", alive)
	}
	// a made-up ID ends nothing
	e.post(t, a, "/account/sessions/end", url.Values{"id": {"0000000000000000"}})
	e.post(t, a, "/account/sessions/end", url.Values{"id": {"x"}})
	// the others all go; this one stays
	if r := e.post(t, a, "/account/sessions/end-others", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("end others: %d", r.StatusCode)
	}
	for name, x := range map[string]*http.Client{"b": b, "c": c} {
		if resp, _ := e.get(t, x, "/account"); resp.StatusCode == http.StatusOK {
			t.Errorf("session %s survived 'log out everywhere else'", name)
		}
	}
	if resp, page := e.get(t, a, "/account/sessions"); resp.StatusCode != http.StatusOK || strings.Count(page, "this one") != 1 || sessionIDRE.MatchString(page) {
		t.Errorf("the session that asked is gone or not alone: %d", resp.StatusCode)
	}
	// the admin's session was not touched by any of it
	if resp, _ := e.get(t, other, "/account"); resp.StatusCode != http.StatusOK {
		t.Error("the admin's session ended")
	}
}
