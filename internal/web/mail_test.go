// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMail struct {
	mu   sync.Mutex
	sent []sentMail
}

type sentMail struct{ to, subject, body string }

func (f *fakeMail) Enabled() bool { return true }
func (f *fakeMail) Send(_ context.Context, to []string, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range to {
		f.sent = append(f.sent, sentMail{t, subject, body})
	}
	return nil
}

// wait returns the n-th mail once it has been sent (mail goes out in the background).
func (f *fakeMail) wait(t *testing.T, n int) sentMail {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		if len(f.sent) >= n {
			m := f.sent[n-1]
			f.mu.Unlock()
			return m
		}
		f.mu.Unlock()
	}
	t.Fatalf("mail %d was not sent", n)
	return sentMail{}
}

func (f *fakeMail) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

var linkRE = regexp.MustCompile(`https?://[^\s]+`)

// linkPath turns the mail's link into a path on the test server.
func linkPath(t *testing.T, body string) string {
	t.Helper()
	l := linkRE.FindString(body)
	if l == "" {
		t.Fatalf("no link in %q", body)
	}
	u, err := url.Parse(l)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}

func newMailEnv(t *testing.T) (*env, *fakeMail) {
	fm := &fakeMail{}
	return newEnvWith(t, func(s *Server) { s.Mailer = fm }), fm
}

func TestSignUpNeedsTheLinkWhenMailIsOn(t *testing.T) {
	e, fm := newMailEnv(t)
	a := e.adminBrowser(t)
	e.post(t, a, "/admin/settings/registration", url.Values{"enabled": {"on"}})
	c := e.browser()
	r := e.post(t, c, "/register", url.Values{"username": {"neo"}, "email": {"neo@example.org"}, "password": {"long enough password"}})
	if body := readBody(r); r.StatusCode != http.StatusOK || !strings.Contains(body, "We sent a link") || len(r.Cookies()) != 0 {
		t.Fatalf("sign-up: %d %v", r.StatusCode, r.Cookies())
	}
	if r := e.login(t, c, "neo", "long enough password"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unverified user logged in: %d", r.StatusCode)
	}
	m := fm.wait(t, 1)
	if m.to != "neo@example.org" || !strings.Contains(m.subject, "Confirm") {
		t.Fatalf("mail: %+v", m)
	}
	path := linkPath(t, m.body)
	// opening the link only asks; nothing is confirmed yet (a mail scanner may open it)
	resp, body := e.get(t, c, path)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `action="/register/verify"`) {
		t.Fatalf("confirm page: %d", resp.StatusCode)
	}
	if r := e.login(t, c, "neo", "long enough password"); r.StatusCode != http.StatusUnauthorized {
		t.Fatal("the GET alone confirmed the address")
	}
	tok := regexp.MustCompile(`name="token" value="([^"]+)"`).FindStringSubmatch(body)[1]
	if r := e.post(t, c, "/register/verify", url.Values{"token": {tok}}); r.StatusCode != http.StatusOK || !strings.Contains(readBody(r), "You can log in now") {
		t.Fatalf("confirm: %d", r.StatusCode)
	}
	if r := e.login(t, c, "neo", "long enough password"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("verified user cannot log in: %d", r.StatusCode)
	}
	// a link works once
	if r := e.post(t, c, "/register/verify", url.Values{"token": {tok}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("link used twice: %d", r.StatusCode)
	}
	if resp, _ := e.get(t, c, "/register/verify?token=nonsense"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad token: %d", resp.StatusCode)
	}
}

func TestVerifiedSignUpStillWaitsForApproval(t *testing.T) {
	e, fm := newMailEnv(t)
	a := e.adminBrowser(t)
	e.post(t, a, "/admin/settings/registration", url.Values{"enabled": {"on"}, "require_approval": {"on"}})
	c := e.browser()
	e.post(t, c, "/register", url.Values{"username": {"trin"}, "email": {"trin@example.org"}, "password": {"long enough password"}})
	_, page := e.get(t, c, linkPath(t, fm.wait(t, 1).body))
	tok := regexp.MustCompile(`name="token" value="([^"]+)"`).FindStringSubmatch(page)[1]
	if body := readBody(e.post(t, c, "/register/verify", url.Values{"token": {tok}})); !strings.Contains(body, "approve") {
		t.Fatalf("expected the approval note: %s", body)
	}
	if r := e.login(t, c, "trin", "long enough password"); r.StatusCode != http.StatusUnauthorized {
		t.Fatal("logged in before approval")
	}
	u := mustUser(t, e, "trin")
	if !u.EmailVerified || !u.PendingApproval || !u.Disabled {
		t.Fatalf("state %+v", u)
	}
	if r := e.post(t, a, "/admin/users/"+u.ID.String()+"/approve", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve: %d", r.StatusCode)
	}
	if r := e.login(t, c, "trin", "long enough password"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("approved: %d", r.StatusCode)
	}
}

func TestResendVerification(t *testing.T) {
	e, fm := newMailEnv(t)
	a := e.adminBrowser(t)
	e.post(t, a, "/admin/settings/registration", url.Values{"enabled": {"on"}})
	c := e.browser()
	e.post(t, c, "/register", url.Values{"username": {"neo"}, "email": {"neo@example.org"}, "password": {"long enough password"}})
	first := fm.wait(t, 1)
	if r := e.post(t, c, "/register/resend", url.Values{"email": {"neo@example.org"}}); r.StatusCode != http.StatusOK {
		t.Fatalf("resend: %d", r.StatusCode)
	}
	second := fm.wait(t, 2)
	if linkPath(t, first.body) == linkPath(t, second.body) {
		t.Error("the old link was sent again")
	}
	if resp, _ := e.get(t, c, linkPath(t, first.body)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("old link still works: %d", resp.StatusCode)
	}
	// an unknown address gets the same answer and no mail
	n := fm.count()
	r := e.post(t, c, "/register/resend", url.Values{"email": {"nobody@example.org"}})
	if body := readBody(r); r.StatusCode != http.StatusOK || !strings.Contains(body, "If that address") {
		t.Fatalf("unknown address: %d", r.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if fm.count() != n {
		t.Error("mail sent to an unknown address")
	}
}

func TestPasswordResetByMail(t *testing.T) {
	e, fm := newMailEnv(t)
	if _, err := e.store.Pool.Exec(ctx, "UPDATE app_user SET email = 'user@example.org' WHERE username = 'user'"); err != nil {
		t.Fatal(err)
	}
	c := e.browser()
	if _, body := e.get(t, c, "/login"); !strings.Contains(body, `href="/forgot"`) {
		t.Error("login page lacks the reset link although mail is on")
	}
	other := e.userBrowser(t) // an existing session must end with the reset

	// unknown address: same answer, no mail
	r := e.post(t, c, "/forgot", url.Values{"email": {"nobody@example.org"}})
	unknown := readBody(r)
	time.Sleep(100 * time.Millisecond)
	if fm.count() != 0 {
		t.Fatal("mail to an unknown address")
	}
	r = e.post(t, c, "/forgot", url.Values{"email": {"user@example.org"}})
	if known := readBody(r); known != unknown || r.StatusCode != http.StatusOK {
		t.Errorf("the answer differs between known and unknown addresses")
	}
	path := linkPath(t, fm.wait(t, 1).body)
	resp, page := e.get(t, c, path)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `action="/reset"`) {
		t.Fatalf("reset page: %d", resp.StatusCode)
	}
	tok := regexp.MustCompile(`name="token" value="([^"]+)"`).FindStringSubmatch(page)[1]
	if r := e.post(t, c, "/reset", url.Values{"token": {tok}, "password": {"short"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("short password accepted: %d", r.StatusCode)
	}
	if r := e.post(t, c, "/reset", url.Values{"token": {tok}, "password": {"a brand new password"}}); r.StatusCode != http.StatusOK {
		t.Fatalf("reset: %d", r.StatusCode)
	}
	if r := e.post(t, c, "/reset", url.Values{"token": {tok}, "password": {"another new password"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("token used twice: %d", r.StatusCode)
	}
	if r := e.login(t, c, "user", "user password"); r.StatusCode != http.StatusUnauthorized {
		t.Error("old password still works")
	}
	if r := e.login(t, c, "user", "a brand new password"); r.StatusCode != http.StatusSeeOther {
		t.Errorf("new password: %d", r.StatusCode)
	}
	if resp, _ := e.get(t, other, "/account"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("an old session survived the reset: %d", resp.StatusCode)
	}
}

func TestNoMailPagesWithoutMail(t *testing.T) {
	e := newEnv(t)
	c := e.browser()
	for _, p := range []string{"/forgot", "/reset?token=x", "/register/verify?token=x"} {
		if resp, _ := e.get(t, c, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s without mail: %d", p, resp.StatusCode)
		}
	}
	if _, body := e.get(t, c, "/login"); strings.Contains(body, "/forgot") {
		t.Error("login page offers a reset that cannot work")
	}
}
