// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// signedIn finds the row "Signed in" (the sidebar says "Signed in as" on every page).
var signedInRow = regexp.MustCompile(`Signed in\s*(<div|</td>)`)

func TestMyActivity(t *testing.T) {
	e := newEnv(t)
	uid, aid := uidOf(e, "user"), uidOf(e, "admin")
	ins := func(actor *uuid.UUID, name, action, objType, objID, details, addr string) {
		t.Helper()
		if _, err := e.store.Pool.Exec(ctx, `INSERT INTO audit_log (actor_user_id, actor_name, action, object_type, object_id, details, remote_addr) VALUES ($1, $2, $3, $4, $5, $6::jsonb, nullif($7, '')::inet)`,
			actor, name, action, objType, objID, details, addr); err != nil {
			t.Fatal(err)
		}
	}
	// about the user: what they did, a failed attempt on their name (any case), what an administrator did to their account
	ins(&uid, "user", "handset.update", "handset", "h1", `{"dual_ring":"busy"}`, "192.0.2.10")
	ins(nil, "anonymous", "user.login_failed", "user", "", `{"username":"USER"}`, "203.0.113.9")
	ins(&aid, "admin", "user.update", "user", uid.String(), `{"changed":"role"}`, "192.0.2.77")
	ins(nil, "system", "user.erased", "user", uid.String(), `{}`, "")
	// about others: an administrator's own work, another user's failed attempt, a change of another user's account, a cell
	ins(&aid, "admin", "settings.update", "setting", "instance", `{"changed":"name"}`, "192.0.2.77")
	ins(nil, "anonymous", "user.login_failed", "user", "", `{"username":"somebody-else"}`, "203.0.113.50")
	ins(&aid, "admin", "user.update", "user", aid.String(), `{"changed":"email"}`, "192.0.2.77")
	ins(&aid, "admin", "cell.enroll", "cell", "c1", `{}`, "192.0.2.77")
	u := e.userBrowser(t) // adds the user's own sign-in
	resp, body := e.get(t, u, "/account/activity")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	if !signedInRow.MatchString(body) {
		t.Error("the user's own sign-in is not in the list")
	}
	for _, want := range []string{"A handset was changed", "dual_ring: busy", "A failed attempt to sign in", "203.0.113.9", "The account was changed", "by admin", "192.0.2.77", "changed: role", "192.0.2.10"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, leak := range []string{"203.0.113.50", "somebody-else", "A base station was enrolled", "changed: email", "changed: name"} {
		if strings.Contains(body, leak) {
			t.Errorf("the page shows what is not the user's: %q", leak)
		}
	}
	if !regexp.MustCompile(`attempt</span>\s*A failed attempt`).MatchString(body) {
		t.Error("a failed attempt is not marked")
	}
	// the filters
	_, access := e.get(t, u, "/account/activity?kind=access")
	if !strings.Contains(access, "A failed attempt to sign in") || !signedInRow.MatchString(access) || strings.Contains(access, "A handset was changed") || strings.Contains(access, "The account was changed") {
		t.Errorf("sign-ins: %s", access)
	}
	_, changes := e.get(t, u, "/account/activity?kind=changes")
	if signedInRow.MatchString(changes) || strings.Contains(changes, "A failed attempt") || !strings.Contains(changes, "A handset was changed") || !strings.Contains(changes, "The account was changed") {
		t.Errorf("changes: %s", changes)
	}
	// an action the page has no words for shows its name
	ins(&uid, "user", "thing.did_it", "thing", "", `{}`, "192.0.2.10")
	if _, b := e.get(t, u, "/account/activity"); !strings.Contains(b, "thing did it") {
		t.Error("an unknown action is not shown")
	}
	// paging: a page holds 50
	for i := 0; i < 60; i++ {
		ins(&uid, "user", "contact.create", "contact", fmt.Sprint(i), `{"label":"c`+fmt.Sprint(i)+`"}`, "192.0.2.10")
	}
	_, page1 := e.get(t, u, "/account/activity")
	if n := strings.Count(page1, "A contact was made"); n != 50 || signedInRow.MatchString(page1) {
		t.Errorf("the first page holds %d of the newest 50", n)
	}
	i := strings.Index(page1, `href="/account/activity?before=`)
	if i < 0 {
		t.Fatal("no link to older entries")
	}
	next := page1[i+len(`href="`) : i+strings.Index(page1[i:], `">Older`)]
	next = strings.ReplaceAll(next, "&amp;", "&")
	_, page2 := e.get(t, u, next)
	if n := strings.Count(page2, "A contact was made"); n != 10 || !signedInRow.MatchString(page2) || strings.Contains(page2, "Older") {
		t.Errorf("the second page holds %d of the other 10 contacts, and the rest", n)
	}
	if resp, _ := e.get(t, e.browser(), "/account/activity"); resp.StatusCode == http.StatusOK {
		t.Error("the page without a login")
	}
	// the account page links to it
	if _, acc := e.get(t, u, "/account"); !strings.Contains(acc, `href="/account/activity"`) {
		t.Error("no link from the account page")
	}
}
