// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
)

// A user takes their data with them (no secrets, nobody else's) and has the account erased with everything it owns (audit finding PRIV-05).
func TestExportAndEraseAccount(t *testing.T) {
	var told []uuid.UUID
	var mu sync.Mutex
	link := &fakeLink{states: map[uuid.UUID]wire.CellStateValue{}}
	e := newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.SIPAccounts, s.SIP = &sipaccount.Store{Pool: s.Pool, Keyring: kr}, fakeSIP{}
		if s.Cells, err = cell.Open(ctx, s.Pool, kr, "dect.example.org:7300"); err != nil {
			t.Fatal(err)
		}
		s.CellLink = link
		s.HandsetDeregister = func(id uuid.UUID, force bool) {
			mu.Lock()
			told = append(told, id)
			mu.Unlock()
		}
	})
	var uid, aid uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'admin'").Scan(&aid)
	var mine, theirs, acct, hall uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x01', 1, '\\x', 'Kitchen') RETURNING id", uid).Scan(&mine)
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x02', 2, '\\x', 'AdminPhone') RETURNING id", aid).Scan(&theirs)
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'HomeLine', '1001', 'sip.example.org') RETURNING id", uid).Scan(&acct)
	e.store.Pool.Exec(ctx, "INSERT INTO handset_line (handset_id, line, sip_account_id) VALUES ($1, 1, $2)", mine, acct)
	e.store.Pool.Exec(ctx, "INSERT INTO contact (owner_user_id, last_name, first_name) VALUES ($1, 'Meyer', 'Eva')", uid)
	e.store.Pool.Exec(ctx, "INSERT INTO call_log (handset_id, owner_user_id, direction, remote_party, started_at) VALUES ($1, $2, 'out', '0301234567', now())", mine, uid)
	e.store.Pool.Exec(ctx, "INSERT INTO call_log (handset_id, owner_user_id, direction, remote_party, started_at) VALUES ($1, $2, 'out', '0409999999', now())", theirs, aid)
	e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state, enrolled_by, location, latitude, longitude) VALUES ('Hall', 'active', $1, 'Workshop', 52.1, 9.6) RETURNING id", uid).Scan(&hall)
	e.store.Pool.Exec(ctx, "INSERT INTO audit_log (actor_user_id, actor_name, action, object_type, remote_addr) VALUES ($1, 'user', 'something.done', 'thing', '10.1.2.3')", uid)
	u := e.userBrowser(t)

	// the export holds the user's things, not the secrets and not other users' things
	r, body := e.get(t, u, "/account/export")
	if r.StatusCode != http.StatusOK || !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export: %d %v", r.StatusCode, r.Header)
	}
	for _, want := range []string{"Kitchen", "HomeLine", "Meyer", "0301234567", "Hall", "Workshop", "something.done"} {
		if !strings.Contains(body, want) {
			t.Errorf("the export lacks %q", want)
		}
	}
	for _, not := range []string{"AdminPhone", "0409999999", "password_hash", "uak", "secret_enc"} {
		if strings.Contains(body, not) {
			t.Errorf("the export holds %q", not)
		}
	}

	// refusals: no tick, wrong password; the last administrator
	if r := e.post(t, u, "/account/erase", url.Values{"password": {"user password"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("erase without the tick: %d", r.StatusCode)
	}
	if r := e.post(t, u, "/account/erase", url.Values{"password": {"wrong"}, "understand": {"on"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("erase with a wrong password: %d", r.StatusCode)
	}
	var n int
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM handset WHERE id = $1", mine).Scan(&n)
	if n != 1 {
		t.Fatal("a refused erasure removed something")
	}
	if r := e.post(t, e.adminBrowser(t), "/account/erase", url.Values{"password": {"admin password"}, "understand": {"on"}}); r.StatusCode != http.StatusConflict {
		t.Errorf("the only administrator erasing themselves: %d", r.StatusCode)
	}

	// the erasure
	if r := e.post(t, u, "/account/erase", url.Values{"password": {"user password"}, "understand": {"on"}}); r.StatusCode != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "/login") {
		t.Fatalf("erase: %d %v", r.StatusCode, r.Header)
	}
	count := func(q string, args ...any) int {
		var c int
		e.store.Pool.QueryRow(ctx, q, args...).Scan(&c)
		return c
	}
	if count("SELECT count(*) FROM app_user WHERE id = $1", uid) != 0 || count("SELECT count(*) FROM handset WHERE owner_user_id = $1", uid) != 0 ||
		count("SELECT count(*) FROM sip_account WHERE owner_user_id = $1", uid) != 0 || count("SELECT count(*) FROM contact WHERE owner_user_id = $1", uid) != 0 {
		t.Error("the account or what it owned is still there")
	}
	if count("SELECT count(*) FROM call_log WHERE remote_party = '0301234567'") != 0 {
		t.Error("the user's call history is still there")
	}
	if count("SELECT count(*) FROM call_log WHERE remote_party = '0409999999'") != 1 || count("SELECT count(*) FROM handset WHERE id = $1", theirs) != 1 {
		t.Error("somebody else's data was touched")
	}
	var state, loc string
	var lat *float64
	e.store.Pool.QueryRow(ctx, "SELECT state, location, latitude FROM cell WHERE id = $1", hall).Scan(&state, &loc, &lat)
	if state != "revoked" || loc != "" || lat != nil || len(link.kicked) != 1 {
		t.Errorf("the user's base station: %q %q %v, kicked %d", state, loc, lat, len(link.kicked))
	}
	mu.Lock()
	if len(told) != 1 || told[0] != mine {
		t.Errorf("cells told to forget: %v", told)
	}
	mu.Unlock()
	if count("SELECT count(*) FROM audit_log WHERE actor_name = 'user' OR remote_addr = '10.1.2.3'") != 0 || count("SELECT count(*) FROM audit_log WHERE actor_name = '(deleted user)' AND action = 'something.done'") != 1 {
		t.Error("the audit log still says who did what")
	}
	if count("SELECT count(*) FROM audit_log WHERE action = 'user.erased'") != 1 {
		t.Error("the erasure is not in the audit log")
	}
	if r := e.login(t, e.browser(), "user", "user password"); r.StatusCode == http.StatusSeeOther {
		t.Error("the erased user logged in")
	}
}
