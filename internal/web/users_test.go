// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestUserAdminPages(t *testing.T) {
	e := newEnv(t)
	admin := e.browser()
	e.login(t, admin, "admin", "admin password")
	r := e.post(t, admin, "/admin/users", url.Values{"username": {"erik"}, "password": {"erik password!"}, "role": {"user"}})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: %d %s", r.StatusCode, readBody(r))
	}
	loc := strings.SplitN(r.Header.Get("Location"), "?", 2)[0]
	if r := e.post(t, admin, "/admin/users", url.Values{"username": {"ERIK"}, "password": {"erik password!"}, "role": {"user"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("duplicate: %d", r.StatusCode)
	}

	erik := e.browser()
	if r := e.login(t, erik, "erik", "erik password!"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("new user login: %d", r.StatusCode)
	}
	// Promote, then disable: erik's session ends.
	if r := e.post(t, admin, loc, url.Values{"role": {"admin"}, "may_add_cells": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: %d", r.StatusCode)
	}
	if u := mustUser(t, e, "erik"); !u.IsAdmin() || !u.MayAddCells {
		t.Fatalf("after update: %+v", u)
	}
	e.post(t, admin, loc, url.Values{"role": {"user"}, "disabled": {"on"}})
	if resp, _ := e.get(t, erik, "/account"); resp.StatusCode != http.StatusSeeOther {
		t.Error("disabled user still logged in")
	}
	// The admin cannot disable or demote themself.
	self := "/admin/users/" + mustUser(t, e, "admin").ID.String()
	if r := e.post(t, admin, self, url.Values{"role": {"user"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("self demote: %d", r.StatusCode)
	}
	// Admin sets a password; a plain user can't reach these pages.
	e.post(t, admin, loc, url.Values{"role": {"user"}})
	if r := e.post(t, admin, loc+"/password", url.Values{"password": {"brand new pw!"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("set password: %d", r.StatusCode)
	}
	if r := e.login(t, e.browser(), "erik", "brand new pw!"); r.StatusCode != http.StatusSeeOther {
		t.Error("new password does not work")
	}
	user := e.browser()
	e.login(t, user, "user", "user password")
	for _, p := range []string{"/admin/users/new", loc} {
		if resp, _ := e.get(t, user, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("user on %s: %d", p, resp.StatusCode)
		}
	}
	if r := e.post(t, user, loc+"/delete", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("user delete: %d", r.StatusCode)
	}
	if r := e.post(t, admin, loc+"/delete", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if resp, _ := e.get(t, admin, loc); resp.StatusCode != http.StatusNotFound {
		t.Error("deleted user still shown")
	}
}
