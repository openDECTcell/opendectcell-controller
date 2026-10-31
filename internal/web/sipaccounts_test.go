// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
)

type fakeSIP struct{}

func (fakeSIP) Status(uuid.UUID) (sip.RegStatus, bool) {
	return sip.RegStatus{State: sip.RegRegistered, Server: "udp:192.0.2.1:5060"}, true
}

func sipFormValues(label, pw string) url.Values {
	return url.Values{"label": {label}, "username": {"301"}, "domain": {"pbx.example.org"}, "password": {pw},
		"transport": {"udp"}, "register": {"on"}, "register_expiry": {"600"}, "keepalive": {"auto"},
		"media_encryption": {"optional"}, "codecs": {"g722, PCMA"}, "dtmf_mode": {"rfc4733"}, "caller_id_source": {"auto"},
		"enabled": {"on"}, "tls_verify": {"on"}}
}

func TestSIPAccountPages(t *testing.T) {
	var store *sipaccount.Store
	e := newEnvWith(t, func(s *Server) {
		mk := testMK
		kr, err := db.LoadKeyring(ctx, s.Pool, mk, nil)
		if err != nil {
			t.Fatal(err)
		}
		store = &sipaccount.Store{Pool: s.Pool, Keyring: kr}
		s.SIPAccounts, s.SIP = store, fakeSIP{}
	})
	user := e.browser()
	e.login(t, user, "user", "user password")

	// Invalid: codec list wrong → 400 with the field message, nothing saved.
	bad := sipFormValues("Home", "pw-very-secret")
	bad.Set("codecs", "GSM")
	r := e.post(t, user, "/sip-accounts", bad)
	body := readBody(r)
	if r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "GSM") {
		t.Fatalf("invalid form: %d", r.StatusCode)
	}
	if strings.Contains(body, "pw-very-secret") {
		t.Fatal("password echoed back into the form")
	}

	r = e.post(t, user, "/sip-accounts", sipFormValues("Home", "pw-very-secret"))
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: %d %s", r.StatusCode, readBody(r))
	}
	loc := r.Header.Get("Location")
	resp, page := e.get(t, user, loc)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "registered") || !strings.Contains(page, "SRTP keys are exchanged") {
		t.Fatalf("edit page: %d", resp.StatusCode)
	}
	if strings.Contains(page, "pw-very-secret") {
		t.Fatal("password rendered")
	}
	if _, list := e.get(t, user, "/sip-accounts"); !strings.Contains(list, "301@pbx.example.org") {
		t.Fatal("list lacks the account")
	}
	id := strings.TrimPrefix(strings.SplitN(loc, "?", 2)[0], "/sip-accounts/")
	rec, err := store.Get(ctx, mustUser(t, e, "user"), uuid.MustParse(id))
	if err != nil || strings.Join(rec.Codecs, ",") != "G722,PCMA" || !rec.HasPassword {
		t.Fatalf("stored %+v %v", rec, err)
	}

	// Editing without a password keeps it.
	form := sipFormValues("Home line", "")
	if r := e.post(t, user, "/sip-accounts/"+id, form); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: %d", r.StatusCode)
	}
	rec, _ = store.Get(ctx, mustUser(t, e, "user"), uuid.MustParse(id))
	if !rec.HasPassword || rec.Label != "Home line" {
		t.Fatalf("after update: %+v", rec)
	}

	// Another user can neither see nor change it; an admin can.
	admin := e.browser()
	e.login(t, admin, "admin", "admin password")
	if _, err := e.store.CreateLocalUser(ctx, newUser("carl")); err != nil {
		t.Fatal(err)
	}
	carl := e.browser()
	e.login(t, carl, "carl", "carl password!")
	for _, p := range []string{"/sip-accounts/" + id} {
		if resp, _ := e.get(t, carl, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("other user GET %s: %d", p, resp.StatusCode)
		}
	}
	if r := e.post(t, carl, "/sip-accounts/"+id+"/delete", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("other user delete: %d", r.StatusCode)
	}
	if resp, page := e.get(t, admin, "/sip-accounts"); resp.StatusCode != http.StatusOK || !strings.Contains(page, ">user<") {
		t.Errorf("admin list: %d", resp.StatusCode)
	}

	if r := e.post(t, user, "/sip-accounts/"+id+"/delete", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if resp, _ := e.get(t, user, "/sip-accounts/"+id); resp.StatusCode != http.StatusNotFound {
		t.Error("deleted account still shown")
	}
}
