// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
)

func TestContactPages(t *testing.T) {
	e := newEnvWith(t, func(s *Server) { s.Contacts = &contacts.Store{Pool: s.Pool} })
	user := e.browser()
	e.login(t, user, "user", "user password")
	if r := e.post(t, user, "/contacts", url.Values{"last_name": {"Meyer"}, "number0": {"030 123"}, "kind0": {"fixed"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("add: %d %s", r.StatusCode, readBody(r))
	}
	if r := e.post(t, user, "/contacts", url.Values{"number0": {"abc"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid: %d", r.StatusCode)
	}
	if r := e.post(t, user, "/contacts", url.Values{"last_name": {"x"}, "shared": {"on"}}); r.StatusCode != http.StatusForbidden {
		t.Errorf("user adding shared: %d", r.StatusCode)
	}
	admin := e.browser()
	e.login(t, admin, "admin", "admin password")
	if r := e.post(t, admin, "/contacts", url.Values{"last_name": {"Reception"}, "number0": {"100"}, "shared": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin shared: %d", r.StatusCode)
	}
	_, page := e.get(t, user, "/contacts")
	if !strings.Contains(page, "Meyer") || !strings.Contains(page, "030123") || !strings.Contains(page, "Reception") {
		t.Fatal("page lacks contacts")
	}
	// A user cannot delete a shared contact; the page says why.
	var shared int64
	e.store.Pool.QueryRow(ctx, "SELECT id FROM contact WHERE owner_user_id IS NULL").Scan(&shared)
	if r := e.post(t, user, "/contacts/"+strconv.FormatInt(shared, 10)+"/delete", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("user deleting shared: %d", r.StatusCode)
	}
}
