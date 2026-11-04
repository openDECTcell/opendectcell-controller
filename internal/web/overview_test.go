// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
)

// statusSIP answers what each line was told to say.
type statusSIP map[uuid.UUID]sip.RegStatus

func (m statusSIP) Status(id uuid.UUID) (sip.RegStatus, bool) {
	st, ok := m[id]
	return st, ok
}

func overviewEnv(t *testing.T, st statusSIP) *env {
	return newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.SIPAccounts, s.SIP = &sipaccount.Store{Pool: s.Pool, Keyring: kr}, st
		if s.Cells, err = cell.Open(ctx, s.Pool, kr, "dect.example.org:7300"); err != nil {
			t.Fatal(err)
		}
	})
}

// The start page says what is wrong, worst first, and says nothing is wrong when nothing is.
func TestOverviewNeedsAttention(t *testing.T) {
	st := statusSIP{}
	e := overviewEnv(t, st)
	var uid, hs, line, ok, station uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	u := e.userBrowser(t)

	// a user with nothing: no problem to report, the first steps are shown
	_, page := e.get(t, u, "/")
	if !strings.Contains(page, "Everything looks fine") || !strings.Contains(page, "Getting started") {
		t.Errorf("an empty overview: %s", page)
	}

	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Home', '1', 'sip.example.org') RETURNING id", uid).Scan(&line)
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Work', '2', 'sip.example.org') RETURNING id", uid).Scan(&ok)
	st[line] = sip.RegStatus{State: sip.RegFailed, LastError: "403 Forbidden"}
	st[ok] = sip.RegStatus{State: sip.RegRegistered}
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x0c', 12, '\\x', 'Kitchen') RETURNING id", uid).Scan(&hs)
	e.store.Pool.Exec(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name, pooled) VALUES ($1, '\\x0e', 14, '\\x', 'Cellar', true)", uid)
	e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state, enrolled_by, last_seen_at) VALUES ('Attic', 'active', $1, now() - interval '3 hours') RETURNING id", uid).Scan(&station)
	cl := &calllog.Store{Pool: e.store.Pool}
	mid, _ := cl.Start(ctx, calllog.Call{OwnerID: &uid, HandsetID: &hs, SIPAccountID: &line, Direction: "in", RemoteParty: "4930777", StartedAt: time.Now().Add(-time.Hour), CellIDs: []uuid.UUID{station}})
	cl.Ended(ctx, mid, calllog.End{At: time.Now().Add(-time.Hour + 20*time.Second), Reason: "no_answer"})
	cl.Start(ctx, calllog.Call{OwnerID: &uid, HandsetID: &hs, SIPAccountID: &ok, Direction: "out", RemoteParty: "4930888", StartedAt: time.Now().Add(-time.Minute), CellIDs: []uuid.UUID{station}})

	_, page = e.get(t, u, "/")
	for _, want := range []string{
		"The VoIP line Home does not register: 403 Forbidden",
		"The base station Attic is offline (last seen 3 hours ago)",
		"The handset Kitchen is not registered on any base station",
		"The handset Cellar has never been heard",
		"The handset Kitchen has no VoIP line, so it cannot call out",
		"1 missed call not seen yet",
		"Calls right now", "4930888", "ringing",
		`<span class="badge bg-success-lt">registered</span>`, `<span class="badge bg-danger-lt">failing</span>`,
		">0 <span class=\"text-secondary fs-4\">of 2</span>", // handsets online
		">0 <span class=\"text-secondary fs-4\">of 1</span>", // base stations online
		"Last calls", "4930777",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the overview lacks %q", want)
		}
	}
	if strings.Contains(page, "Everything looks fine") || strings.Contains(page, "This controller") {
		t.Errorf("a problem list that says all is well, or the administrators' card for a user")
	}
	// worst first: a failing line and an offline base station before a missed call before a handset note
	i, j, k := strings.Index(page, "does not register"), strings.Index(page, "1 missed call"), strings.Index(page, "Cellar has never been heard")
	if i >= j || j >= k {
		t.Errorf("order of the attention list: %d %d %d", i, j, k)
	}
	// the base station shows the call going on and what it carries
	if !strings.Contains(page, "<td>1") {
		t.Errorf("no call counted on the base station")
	}
}

func TestOverviewHealthyAndAdmin(t *testing.T) {
	st := statusSIP{}
	e := overviewEnv(t, st)
	var uid, hs, line uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Home', '1', 'sip.example.org') RETURNING id", uid).Scan(&line)
	st[line] = sip.RegStatus{State: sip.RegRegistered}
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name, pooled, last_seen_at) VALUES ($1, '\\x0d', 13, '\\x', 'Hall', true, now()) RETURNING id", uid).Scan(&hs)
	e.store.Pool.Exec(ctx, "INSERT INTO handset_line (handset_id, line, sip_account_id) VALUES ($1, 1, $2)", hs, line)
	_, page := e.get(t, e.userBrowser(t), "/")
	if !strings.Contains(page, "Everything looks fine") || strings.Contains(page, "has never been heard") {
		t.Errorf("a healthy setup has complaints: %s", page)
	}
	// an administrator also sees the controller, and what is theirs to fix, and not other people's handsets
	_, ap := e.get(t, e.adminBrowser(t), "/")
	if strings.Contains(ap, "Hall") || strings.Contains(ap, ">Home<") {
		t.Errorf("the administrator's overview shows a user's handset or line")
	}
	for _, want := range []string{"This controller", "Version", "Database", "No mail server is set up"} {
		if !strings.Contains(ap, want) {
			t.Errorf("the administrators' overview lacks %q", want)
		}
	}
}

// The administrators' dashboard counts the calls going on, and on which base station.
func TestAdminDashboardShowsCallsNow(t *testing.T) {
	e := overviewEnv(t, statusSIP{})
	var uid, station uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state, enrolled_by) VALUES ('Attic', 'active', $1) RETURNING id", uid).Scan(&station)
	cl := &calllog.Store{Pool: e.store.Pool}
	for _, ago := range []time.Duration{time.Minute, 2 * time.Minute} {
		cl.Start(ctx, calllog.Call{OwnerID: &uid, Direction: "out", RemoteParty: "1", StartedAt: time.Now().Add(-ago), CellIDs: []uuid.UUID{station}})
	}
	// an old call whose end was never written is not going on
	cl.Start(ctx, calllog.Call{OwnerID: &uid, Direction: "out", RemoteParty: "2", StartedAt: time.Now().Add(-8 * time.Hour), CellIDs: []uuid.UUID{station}})
	_, page := e.get(t, e.adminBrowser(t), "/admin")
	if !strings.Contains(page, "<strong>2 going on now</strong>") || !strings.Contains(page, "<td>2</td>") {
		t.Errorf("the dashboard does not show two calls going on: %s", page)
	}
}
