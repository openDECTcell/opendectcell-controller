// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestHandsetPages(t *testing.T) {
	var changedMu sync.Mutex
	var changed, deregistered []uuid.UUID
	var forced []bool
	e := newEnvWith(t, func(s *Server) {
		s.HandsetChanged = func(id uuid.UUID) {
			changedMu.Lock()
			changed = append(changed, id)
			changedMu.Unlock()
		}
		s.HandsetDeregister = func(id uuid.UUID, force bool) {
			changedMu.Lock()
			deregistered = append(deregistered, id)
			forced = append(forced, force)
			changedMu.Unlock()
		}
	})
	var uid, aid uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'admin'").Scan(&aid)
	var mine, theirs, acct, shared, foreign uuid.UUID
	ins := func(owner uuid.UUID, tpui int, name string) uuid.UUID {
		var id uuid.UUID
		if err := e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name, model) VALUES ($1, $2, $3, '\\x', $4, 'Gigaset A540') RETURNING id",
			owner, []byte{byte(tpui)}, tpui, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mine, theirs = ins(uid, 1, "Kitchen"), ins(aid, 2, "Admin phone")
	if err := e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Home', '1001', 'sip.example.org') RETURNING id", uid).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES (NULL, 'Shared', '1002', 'sip.example.org') RETURNING id").Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Admin private', '1003', 'sip.example.org') RETURNING id", aid).Scan(&foreign); err != nil {
		t.Fatal(err)
	}

	var hall uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('Hall', 'active') RETURNING id").Scan(&hall)
	var barn uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('Barn', 'active') RETURNING id").Scan(&barn)
	e.store.Pool.Exec(ctx, "INSERT INTO handset_move (handset_id, from_cell_id, to_cell_id, at) VALUES ($1, $2, $3, now())", mine, hall, barn)

	u, a := e.userBrowser(t), e.adminBrowser(t)
	_, mv := e.get(t, u, "/handsets/"+mine.String())
	if !strings.Contains(mv, "not registered on any cell") {
		t.Errorf("a handset no cell holds is not marked: %s", mv)
	}
	e.store.Pool.Exec(ctx, "INSERT INTO cell_slot (cell_id, slot, handset_id, state) VALUES ($1, 1, $2, 'present')", hall, mine)
	_, mv = e.get(t, u, "/handsets/"+mine.String())
	if !strings.Contains(mv, "home cell only") || !strings.Contains(mv, "Hall") || !strings.Contains(mv, "Barn") {
		t.Errorf("the handset page shows no cell change or its kind: %s", mv)
	}
	e.store.Pool.Exec(ctx, "UPDATE handset SET pooled = true WHERE id = $1", mine)
	if _, mv := e.get(t, u, "/handsets/"+mine.String()); !strings.Contains(mv, ">every cell<") {
		t.Errorf("a pooled handset is not marked: %s", mv)
	}
	_, list := e.get(t, u, "/handsets")
	if !strings.Contains(list, "Kitchen") || strings.Contains(list, "Admin phone") {
		t.Errorf("a user's list: %s", list)
	}
	_, all := e.get(t, a, "/handsets")
	if !strings.Contains(all, "Kitchen") || !strings.Contains(all, "Admin phone") || !strings.Contains(all, "<th>Owner</th>") {
		t.Error("the admin's list lacks someone's handset or the owner column")
	}
	if resp, _ := e.get(t, u, "/handsets/"+theirs.String()); resp.StatusCode != http.StatusNotFound {
		t.Errorf("someone else's handset: %d", resp.StatusCode)
	}
	if r := e.post(t, u, "/handsets/"+theirs.String(), url.Values{"name": {"mine"}, "dual_ring": {"busy"}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("edit of someone else's handset: %d", r.StatusCode)
	}
	if r := e.post(t, u, "/handsets/"+theirs.String()+"/lines/1", url.Values{"account": {acct.String()}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("line on someone else's handset: %d", r.StatusCode)
	}
	// lines: own and shared accounts yes, someone else's no
	for _, id := range []uuid.UUID{acct, shared} {
		if r := e.post(t, u, "/handsets/"+mine.String()+"/lines/1", url.Values{"account": {id.String()}}); r.StatusCode != http.StatusSeeOther {
			t.Fatalf("set line: %d", r.StatusCode)
		}
	}
	if r := e.post(t, u, "/handsets/"+mine.String()+"/lines/2", url.Values{"account": {foreign.String()}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("someone else's account accepted: %d", r.StatusCode)
	}
	if r := e.post(t, u, "/handsets/"+mine.String()+"/lines/3", url.Values{"account": {acct.String()}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("line 3 accepted: %d", r.StatusCode)
	}
	_, page := e.get(t, u, "/handsets/"+mine.String())
	if !strings.Contains(page, "Shared") || strings.Contains(page, "Admin private") {
		t.Errorf("handset page lists the wrong accounts: %s", page)
	}
	// name, default line (must be a line it has), ring policy
	if r := e.post(t, u, "/handsets/"+mine.String(), url.Values{"name": {"Kitchen phone"}, "default_line": {"2"}, "dual_ring": {"busy"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("default line 2 without a line 2: %d", r.StatusCode)
	}
	if r := e.post(t, u, "/handsets/"+mine.String(), url.Values{"name": {"Kitchen phone"}, "default_line": {"1"}, "dual_ring": {"busy"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", r.StatusCode)
	}
	if r := e.post(t, u, "/handsets/"+mine.String(), url.Values{"name": {"x"}, "dual_ring": {"shout"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("bad ring policy: %d", r.StatusCode)
	}
	// the device profile: only the catalogue's, and the stacks are told
	if r := e.post(t, u, "/handsets/"+mine.String(), url.Values{"name": {"Kitchen phone"}, "default_line": {"1"}, "dual_ring": {"busy"}, "profile": {"nokia-3310"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown profile: %d", r.StatusCode)
	}
	if r := e.post(t, u, "/handsets/"+mine.String(), url.Values{"name": {"Kitchen phone"}, "default_line": {"1"}, "dual_ring": {"busy"}, "profile": {"gigaset-c430a"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("a known profile: %d", r.StatusCode)
	}
	var prof, model string
	e.store.Pool.QueryRow(ctx, "SELECT coalesce(profile, ''), coalesce(model, '') FROM handset WHERE id = $1", mine).Scan(&prof, &model)
	if prof != "gigaset-c430a" || model != "Gigaset C430A" {
		t.Errorf("stored profile %q model %q", prof, model)
	}
	changedMu.Lock()
	if len(changed) == 0 || changed[len(changed)-1] != mine {
		t.Errorf("the stacks were not told: %v", changed)
	}
	changedMu.Unlock()
	if _, pg := e.get(t, u, "/handsets/"+mine.String()); !strings.Contains(pg, `value="gigaset-c430a" selected`) {
		t.Error("the profile is not selected on the page")
	}
	// phone lines are the owner's: an admin sees the handset, not its lines
	e.store.Pool.Exec(ctx, "INSERT INTO handset_line (handset_id, line, sip_account_id) VALUES ($1, 1, $2) ON CONFLICT DO NOTHING", mine, acct)
	if _, ap := e.get(t, a, "/handsets/"+mine.String()); strings.Contains(ap, "Line 1") || strings.Contains(ap, ">Home<") || !strings.Contains(ap, "private to the handset") {
		t.Errorf("an admin sees the owner's lines: %s", ap)
	}
	if _, al := e.get(t, a, "/handsets"); strings.Contains(al, "1: Home") || !strings.Contains(al, "private") {
		t.Error("the admin's list shows the owner's lines")
	}
	if r := e.post(t, a, "/handsets/"+mine.String()+"/lines/1", url.Values{"account": {""}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("an admin changed the owner's line: %d", r.StatusCode)
	}
	// saving the name as an admin leaves the owner's default line alone
	e.store.Pool.Exec(ctx, "UPDATE handset SET default_line = 1 WHERE id = $1", mine)
	if r := e.post(t, a, "/handsets/"+mine.String(), url.Values{"name": {"Kitchen phone"}, "dual_ring": {"busy"}}); r.StatusCode != http.StatusSeeOther {
		t.Errorf("admin save: %d", r.StatusCode)
	}
	var kept *int
	e.store.Pool.QueryRow(ctx, "SELECT default_line FROM handset WHERE id = $1", mine).Scan(&kept)
	if kept == nil || *kept != 1 {
		t.Error("an admin's save cleared the owner's default line")
	}
	// the IPEI is on the list and the page
	if _, al := e.get(t, u, "/handsets"); !strings.Contains(al, "<th>IPEI</th>") {
		t.Error("no IPEI column")
	}
	// removing: the owner's handset only; the cells are asked, the row goes
	if r := e.post(t, u, "/handsets/"+theirs.String()+"/deregister", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("removing someone else's handset: %d", r.StatusCode)
	}
	old, older := ins(uid, 3, "Old phone"), ins(uid, 4, "Older phone")
	e.store.Pool.Exec(ctx, "INSERT INTO cell_slot (cell_id, slot, handset_id, state) VALUES ($1, 2, $2, 'present')", hall, old)
	if r := e.post(t, u, "/handsets/"+old.String()+"/deregister", nil); r.StatusCode != http.StatusSeeOther {
		t.Errorf("remove: %d", r.StatusCode)
	}
	if r := e.post(t, u, "/handsets/"+older.String()+"/deregister", url.Values{"force": {"1"}}); r.StatusCode != http.StatusSeeOther {
		t.Errorf("forced remove: %d", r.StatusCode)
	}
	changedMu.Lock()
	if len(deregistered) != 2 || deregistered[0] != old || deregistered[1] != older || forced[0] || !forced[1] {
		t.Errorf("the cells were asked for %v %v", deregistered, forced)
	}
	changedMu.Unlock()
	var n int
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM handset WHERE id = ANY($1)", []uuid.UUID{old, older}).Scan(&n)
	if n != 0 {
		t.Errorf("%d removed handsets are still in the list", n)
	}
	var state string
	e.store.Pool.QueryRow(ctx, "SELECT state FROM cell_slot WHERE cell_id = $1 AND slot = 2", hall).Scan(&state)
	if state != "empty" {
		t.Errorf("the slot of a removed handset is %q", state)
	}
	if resp, _ := e.get(t, u, "/handsets/"+old.String()); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the page of a removed handset: %d", resp.StatusCode)
	}
	var name, ring string
	var def *int
	e.store.Pool.QueryRow(ctx, "SELECT display_name, dual_ring_policy, default_line FROM handset WHERE id = $1", mine).Scan(&name, &ring, &def)
	if name != "Kitchen phone" || ring != "busy" || def == nil || *def != 1 {
		t.Errorf("stored %q %q %v", name, ring, def)
	}
	// clearing line 1 clears the default
	if r := e.post(t, u, "/handsets/"+mine.String()+"/lines/1", url.Values{"account": {""}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear line: %d", r.StatusCode)
	}
	e.store.Pool.QueryRow(ctx, "SELECT default_line FROM handset WHERE id = $1", mine).Scan(&def)
	if def != nil {
		t.Error("the default line survived its line")
	}
	// the admin may edit anyone's
	if r := e.post(t, a, "/handsets/"+mine.String(), url.Values{"name": {"Kitchen"}, "dual_ring": {"call_waiting"}}); r.StatusCode != http.StatusSeeOther {
		t.Errorf("admin edit: %d", r.StatusCode)
	}
	if resp, body := e.get(t, u, "/handsets/register"); resp.StatusCode != http.StatusOK || !strings.Contains(body, `id="reg-go"`) ||
		!strings.Contains(body, `id="reg-name"`) || !strings.Contains(body, "Gigaset A540") || !strings.Contains(body, "Manufacturer") {
		t.Errorf("register page: %d", resp.StatusCode)
	}
}
