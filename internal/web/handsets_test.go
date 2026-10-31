// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestHandsetPages(t *testing.T) {
	e := newEnv(t)
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

	u, a := e.userBrowser(t), e.adminBrowser(t)
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
	if resp, body := e.get(t, u, "/handsets/register"); resp.StatusCode != http.StatusOK || !strings.Contains(body, `id="reg-go"`) {
		t.Errorf("register page: %d", resp.StatusCode)
	}
}
