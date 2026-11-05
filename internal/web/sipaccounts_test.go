// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

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
	// an admin does not see another user's lines either
	if resp, page := e.get(t, admin, "/sip-accounts"); resp.StatusCode != http.StatusOK || strings.Contains(page, "/sip-accounts/"+id) {
		t.Errorf("admin list: %d, shows the user's account", resp.StatusCode)
	}
	if resp, _ := e.get(t, admin, "/sip-accounts/"+id); resp.StatusCode != http.StatusNotFound {
		t.Errorf("admin GET of another user's account: %d", resp.StatusCode)
	}

	if r := e.post(t, user, "/sip-accounts/"+id+"/delete", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if resp, _ := e.get(t, user, "/sip-accounts/"+id); resp.StatusCode != http.StatusNotFound {
		t.Error("deleted account still shown")
	}
}

// profileForm is what the VoIP line page posts with every box and menu
// set the way a user coming from LuCI would set them.
func profileForm() url.Values {
	return url.Values{"label": {"Fritz"}, "line_name": {"Home"}, "display_name": {"Paula"}, "username": {"621"},
		"auth_username": {"login621"}, "password": {"sip-pw-secret"}, "registrar": {"192.168.178.1:5060"}, "domain": {""},
		"register": {"on"}, "register_expiry": {"900"},
		"transport": {"tcp"}, "outbound_proxy": {"proxy.example.net"}, "outbound2": {"proxy2.example.net:5070"}, "sipnat": {"on"},
		"medianat": {"turn"}, "stunserver": {"turn.example.net:3478"}, "stunuser": {"turner"}, "stunpass": {"turn-pw-secret"},
		"rtpkeep": {"zero"},
		"codec":   {"PCMU", "G722"}, "ptime": {"30"}, "dtmf_mode": {"auto"}, "rel100": {"required"}, "mwi": {"on"},
		"enc_auto": {""}, "mediaenc": {"srtp-mandf"},
		"extra": {";answermode=auto"}, "caller_id_source": {"pai"}, "keepalive": {"crlf"}, "user_agent": {"Test/1"},
		"tls_verify": {"on"}, "enabled": {"on"}}
}

func newSIPEnv(t *testing.T) (*env, *sipaccount.Store, *http.Client) {
	var store *sipaccount.Store
	e := newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		store = &sipaccount.Store{Pool: s.Pool, Keyring: kr}
		s.SIPAccounts, s.SIP = store, fakeSIP{}
	})
	user := e.browser()
	e.login(t, user, "user", "user password")
	return e, store, user
}

func TestVoIPLineForm(t *testing.T) {
	e, store, user := newSIPEnv(t)
	me := mustUser(t, e, "user")

	// The page has LuCI's tabs and field titles.
	resp, page := e.get(t, user, "/sip-accounts/new")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new: %d", resp.StatusCode)
	}
	for _, want := range []string{"New VoIP line", ">Account<", ">Server and NAT<", ">Media<", ">Encryption<", ">Advanced<",
		"Line name", "Name on the handset", "Display name", "User name", "Login name", ">Password<", ">Registrar<", "SIP domain",
		"Registration interval", "UDP", "TCP", "TLS (encrypted signalling)", "Outbound proxy", "Second outbound proxy",
		"SIP outbound (RFC 5626)", "NAT traversal for media", "STUN/TURN server", "TURN user", "TURN password", "Keep media open",
		"Try to autodetect", "G.722 (HD)", "G.711 A-law", "G.711 µ-law", "Packet time", "Key tones (DTMF)", "RTP events (RFC 4733)",
		"SIP INFO", "Reliable provisional responses (100rel)", "Voicemail notification (MWI)", "Allow call transfer",
		"SRTP when offered", "SRTP required (in the offer)", "DTLS-SRTP", "Further account parameters",
		"Stored; this controller does not use it yet."} {
		if !strings.Contains(page, want) {
			t.Errorf("new page lacks %q", want)
		}
	}
	if strings.Contains(page, "SIP account") || strings.Contains(page, "Phone lines") {
		t.Error("the old names are still on the page")
	}

	// Create with every option set; the registrar's host becomes the domain.
	r := e.post(t, user, "/sip-accounts", profileForm())
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: %d %s", r.StatusCode, readBody(r))
	}
	loc := r.Header.Get("Location")
	id := uuid.MustParse(strings.TrimPrefix(strings.SplitN(loc, "?", 2)[0], "/sip-accounts/"))
	rec, err := store.Get(ctx, me, id)
	if err != nil {
		t.Fatal(err)
	}
	a := rec.Advanced
	if rec.Domain != "192.168.178.1" || rec.Registrar != "192.168.178.1:5060" || rec.Username != "621" || rec.AuthUsername != "login621" ||
		rec.Transport != "tcp" || rec.RegisterExpiry != 900 || rec.OutboundProxy != "proxy.example.net" || !rec.HasPassword || !rec.HasStunPass {
		t.Errorf("columns: %+v", rec)
	}
	if strings.Join(rec.Codecs, ",") != "G722,PCMU" || rec.DTMFMode != "rfc4733" || rec.MediaEncryption != "mandatory" || rec.DTMFChoice() != "auto" ||
		rec.EncChoice() != "srtp-mandf" || rec.MediaAutoOn() || rec.EncAutoOn() {
		t.Errorf("media: codecs %v dtmf %s/%s enc %s/%s", rec.Codecs, rec.DTMFMode, rec.DTMFChoice(), rec.MediaEncryption, rec.EncChoice())
	}
	if a.LineName != "Home" || a.Outbound2 != "proxy2.example.net:5070" || !a.SIPNat || a.MediaNat != "turn" || a.StunServer != "turn.example.net:3478" ||
		a.StunUser != "turner" || a.RTPKeep != "zero" || a.PTime != "30" || a.Rel100 != "required" || !a.MWI || a.TransferOn() ||
		a.Extra != ";answermode=auto" || a.UserAgent != "Test/1" {
		t.Errorf("advanced: %+v", a)
	}

	// The edit page shows it back, and no secret.
	resp, page = e.get(t, user, loc)
	if resp.StatusCode != http.StatusOK || strings.Contains(page, "sip-pw-secret") || strings.Contains(page, "turn-pw-secret") {
		t.Fatalf("edit: %d, secrets %v", resp.StatusCode, strings.Contains(page, "-secret"))
	}
	for _, want := range []string{`value="Fritz"`, `value="Home"`, `value="192.168.178.1:5060"`, `value="turn.example.net:3478"`, `value="login621"`,
		`<option value="srtp-mandf" selected>`, `<option value="auto" selected>`, `<option value="30" selected>`, `<option value="turn" selected>`,
		`<option value="required" selected>`, "Remove the stored TURN password", "Remove the stored password"} {
		if !strings.Contains(page, want) {
			t.Errorf("edit page lacks %q", want)
		}
	}
	if strings.Contains(page, `name="transfer" checked`) || !strings.Contains(page, `name="mwi" checked`) || strings.Contains(page, `name="media_auto" checked`) {
		t.Error("the boxes do not show what was stored")
	}

	// Saving again without passwords keeps both; what the form does not carry is kept.
	adv := rec.Advanced
	adv.SessionTimers, adv.SessionExpires = true, 1800
	if _, err := store.Update(ctx, me, id, sipaccount.Input{Advanced: &adv}); err != nil {
		t.Fatal(err)
	}
	f := profileForm()
	f.Del("password")
	f.Del("stunpass")
	f.Set("enc_auto", "on")
	f.Set("media_auto", "on")
	f.Set("transfer", "on")
	f.Set("registrar", "pbx.example.org")
	f.Set("domain", "pbx.example.org")
	if r := e.post(t, user, "/sip-accounts/"+id.String(), f); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: %d %s", r.StatusCode, readBody(r))
	}
	rec, _ = store.Get(ctx, me, id)
	if !rec.HasPassword || !rec.HasStunPass || rec.Registrar != "" || rec.Domain != "pbx.example.org" {
		t.Errorf("after update: %+v", rec)
	}
	if !rec.Advanced.SessionTimers || rec.Advanced.SessionExpires != 1800 {
		t.Errorf("advanced fields the form lacks were lost: %+v", rec.Advanced)
	}
	if strings.Join(rec.Codecs, ",") != "G722,PCMA,PCMU" || rec.MediaEncryption != "optional" || !rec.EncAutoOn() || !rec.MediaAutoOn() || !rec.Advanced.TransferOn() {
		t.Errorf("autodetect: codecs %v enc %s", rec.Codecs, rec.MediaEncryption)
	}
	f.Set("clear_stunpass", "on")
	f.Set("clear_password", "on")
	e.post(t, user, "/sip-accounts/"+id.String(), f)
	if rec, _ = store.Get(ctx, me, id); rec.HasPassword || rec.HasStunPass {
		t.Errorf("passwords not removed: %+v", rec)
	}
}

func TestVoIPLineFormErrors(t *testing.T) {
	e, store, user := newSIPEnv(t)
	me := mustUser(t, e, "user")
	for _, c := range []struct{ key, val, tab, msg string }{
		{"line_name", "Sechzehn Zeichen!", "acct", "at most 15"},
		{"outbound2", "bad host", "net", "host name or address"},
		{"stunserver", "a@b", "net", "host name or address"},
		{"extra", "answermode=auto", "adv", "key=value"},
		{"register_expiry", "soon", "acct", "number of seconds"},
		{"country_code", "+49", "num", "country code is one to three digits"},
		{"strip_prefix", "0", "num", "must differ from the national and international prefixes"},
	} {
		f := profileForm()
		f.Set(c.key, c.val)
		r := e.post(t, user, "/sip-accounts", f)
		body := readBody(r)
		if r.StatusCode != http.StatusBadRequest || !strings.Contains(body, c.msg) {
			t.Errorf("%s: %d", c.key, r.StatusCode)
			continue
		}
		// the page opens on the tab with the mistake, and marks it
		if !strings.Contains(body, `nav-link text-nowrap active text-danger" href="#tab-`+c.tab+`"`) {
			t.Errorf("%s: the %s tab is not the one open", c.key, c.tab)
		}
		if strings.Contains(body, "sip-pw-secret") || strings.Contains(body, "turn-pw-secret") {
			t.Errorf("%s: a password was echoed", c.key)
		}
		if c.key != "register_expiry" && !strings.Contains(body, c.val) {
			t.Errorf("%s: what was typed is gone", c.key)
		}
	}
	// no codec at all, with autodetect off
	f := profileForm()
	f.Del("codec")
	if r := e.post(t, user, "/sip-accounts", f); r.StatusCode != http.StatusBadRequest {
		t.Errorf("no codec: %d", r.StatusCode)
	}
	if l, _ := store.List(ctx, me, ""); len(l) != 0 {
		t.Errorf("%d accounts saved from bad forms", len(l))
	}
	// no domain and no registrar
	f = profileForm()
	f.Set("registrar", "")
	if r := e.post(t, user, "/sip-accounts", f); r.StatusCode != http.StatusBadRequest {
		t.Errorf("no registrar and no domain: %d", r.StatusCode)
	}
}

func TestVoIPLineList(t *testing.T) {
	e, store, user := newSIPEnv(t)
	me := mustUser(t, e, "user")
	a, err := store.Create(ctx, me, sipaccount.Input{Label: ptr("Home"), Username: ptr("301"), Domain: ptr("sip.example.org"),
		Registrar: ptr("sip.example.org:5070"), Transport: ptr("udp"), MediaEncryption: ptr("off")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, me, sipaccount.Input{Label: ptr("Spare"), Username: ptr("302"), Domain: ptr("pbx.example.org"),
		Transport: ptr("udp"), MediaEncryption: ptr("off")}); err != nil {
		t.Fatal(err)
	}
	var h uuid.UUID
	e.app.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x01', 1, '\\x', 'Kitchen') RETURNING id", me.ID).Scan(&h)
	e.app.Pool.Exec(ctx, "INSERT INTO handset_line (handset_id, line, sip_account_id) VALUES ($1, 2, $2)", h, a.ID)

	resp, page := e.get(t, user, "/sip-accounts")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	for _, want := range []string{"VoIP lines", "Line name", "Registration", "Used by", "301@sip.example.org:5070", "302@pbx.example.org",
		"Kitchen, Line 2", "/handsets/" + h.String(), "free", "registered"} {
		if !strings.Contains(page, want) {
			t.Errorf("list lacks %q", want)
		}
	}
	// the menu and the handset page use the same name
	if !strings.Contains(page, ">VoIP lines<") {
		t.Error("the menu does not say VoIP lines")
	}
	if _, hp := e.get(t, user, "/handsets/"+h.String()); !strings.Contains(hp, "Line 1") || !strings.Contains(hp, "Line 2") {
		t.Error("the handset page lacks Line 1 and Line 2")
	}
}

func ptr[T any](v T) *T { return &v }

// The Numbers tab keeps what it is given, shows it again, and puts it into the stored line.
func TestVoIPLineNumbersTab(t *testing.T) {
	e, store, user := newSIPEnv(t)
	f := profileForm()
	for k, v := range map[string]string{"country_code": "49", "area_code": "30", "trunk_prefix": "", "intl_prefix": "00", "strip_prefix": "9",
		"dial_as": "e164", "show_as": "national"} {
		f.Set(k, v)
	}
	r := e.post(t, user, "/sip-accounts", f)
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: %d %s", r.StatusCode, readBody(r))
	}
	loc := r.Header.Get("Location")
	id := uuid.MustParse(strings.TrimPrefix(strings.SplitN(loc, "?", 2)[0], "/sip-accounts/"))
	rec, err := store.Get(ctx, mustUser(t, e, "user"), id)
	if err != nil {
		t.Fatal(err)
	}
	n := rec.Advanced.Numbers
	if n.CountryCode != "49" || n.AreaCode != "30" || n.StripPrefix != "9" || n.DialAs != "e164" || n.ShowAs != "national" {
		t.Errorf("stored %+v", n)
	}
	// the SIP side gets them
	accts, err := store.RuntimeAccounts(ctx)
	if err != nil || len(accts) != 1 || accts[0].Numbers.Dial("0301234567") != "+49301234567" {
		t.Errorf("the SIP account does not carry the rules: %+v %v", accts, err)
	}
	_, page := e.get(t, user, loc)
	for _, want := range []string{`name="country_code" value="49"`, `name="area_code" value="30"`, `name="strip_prefix" value="9"`, `<option value="e164" selected>`, `<option value="national" selected>`, ">Numbers<"} {
		if !strings.Contains(page, want) {
			t.Errorf("the form lacks %q", want)
		}
	}
	// emptied again, the rules are gone
	g := profileForm()
	r = e.post(t, user, loc, g)
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear: %d %s", r.StatusCode, readBody(r))
	}
	rec, _ = store.Get(ctx, mustUser(t, e, "user"), id)
	if !rec.Advanced.Numbers.Zero() {
		t.Errorf("rules after clearing: %+v", rec.Advanced.Numbers)
	}
}

// A line's page tells how its registration went (the history and the share of time it was up) and shows its last calls: the user's
// own, never another user's on a shared line.
func TestVoIPLineHistoryAndCalls(t *testing.T) {
	var store *sipaccount.Store
	e := newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		store = &sipaccount.Store{Pool: s.Pool, Keyring: kr}
		s.SIPAccounts, s.SIP = store, fakeSIP{}
	})
	user := e.userBrowser(t)
	r := e.post(t, user, "/sip-accounts", sipFormValues("Home", "pw"))
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: %d", r.StatusCode)
	}
	var id, uid uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM sip_account").Scan(&id)
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	loc := "/sip-accounts/" + id.String()

	// nothing recorded yet: it says so, and the page works
	if resp, body := e.get(t, user, loc); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Registration history") || !strings.Contains(body, "Nothing is on record yet") ||
		!strings.Contains(body, "No calls on this line yet") {
		t.Fatalf("empty history: %d", resp.StatusCode)
	}

	now := time.Now()
	ev := func(ago time.Duration, state, msg string, took int) {
		e.store.Pool.Exec(ctx, "INSERT INTO sip_reg_event (account_id, at, state, error, took_ms) VALUES ($1, $2, $3, $4, $5)", id, now.Add(-ago), state, msg, took)
	}
	ev(48*time.Hour, "registered", "", 300)
	ev(5*time.Hour, "failed", "403 Forbidden", 0)
	ev(4*time.Hour, "backoff", "timeout talking to the server", 0)
	ev(1*time.Hour, "registered", "", 180)
	// a call of the user's on this line, and one of somebody else's on the same line
	e.store.Pool.Exec(ctx, `INSERT INTO call_log (owner_user_id, sip_account_id, direction, remote_party, started_at, answered_at, ended_at)
		VALUES ($1, $2, 'in', '+4930111222', now() - interval '10 minutes', now() - interval '9 minutes', now() - interval '5 minutes')`, uid, id)
	var other uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO app_user (username, role, auth_source) VALUES ('stranger', 'user', 'local') RETURNING id").Scan(&other)
	e.store.Pool.Exec(ctx, `INSERT INTO call_log (owner_user_id, sip_account_id, direction, remote_party, started_at)
		VALUES ($1, $2, 'out', '0049999888777', now() - interval '20 minutes')`, other, id)

	resp, body := e.get(t, user, loc)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	for _, want := range []string{"403 Forbidden", "timeout talking to the server", "refused", "not reachable", "registered", "180 ms", "Went down <strong>1</strong> time.", "4930111222", "answered"} {
		if !strings.Contains(strings.ReplaceAll(body, "\n", " "), want) && !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	// 7 days window: up from -48 h to -5 h, down -5 h to -1 h, up -1 h to now: 44 of 48 h
	if !strings.Contains(body, "91.7") {
		t.Errorf("the share of time up is not on the page (want 91.7 %%)")
	}
	if strings.Contains(body, "0049999888777") {
		t.Error("another user's call on the line is shown")
	}
}
