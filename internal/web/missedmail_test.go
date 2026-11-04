// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
)

func TestMissedCallMail(t *testing.T) {
	e, fm := newMailEnv(t)
	var uid, other uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'admin'").Scan(&other)
	e.store.Pool.Exec(ctx, "UPDATE app_user SET email = 'user@example.org' WHERE id = $1", uid)
	e.store.Pool.Exec(ctx, "UPDATE app_user SET email = 'admin@example.org' WHERE id = $1", other)
	var line, hs uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Home', '1', 'sip.example.org') RETURNING id", uid).Scan(&line)
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x0b', 11, '\\x', 'Kitchen') RETURNING id", uid).Scan(&hs)
	cl := &calllog.Store{Pool: e.store.Pool}
	missed := func(owner uuid.UUID, number string, endedAgo time.Duration, answered bool) {
		t.Helper()
		start := time.Now().Add(-endedAgo - 20*time.Second)
		id, err := cl.Start(ctx, calllog.Call{OwnerID: &owner, HandsetID: &hs, SIPAccountID: &line, Direction: "in", RemoteParty: number, StartedAt: start})
		if err != nil {
			t.Fatal(err)
		}
		if answered {
			cl.Answered(ctx, id, start.Add(time.Second))
		}
		if err := cl.Ended(ctx, id, calllog.End{At: start.Add(20 * time.Second), Reason: "normal"}); err != nil {
			t.Fatal(err)
		}
	}
	look := func() error { return e.app.sendMissedCallMails(ctx, time.Now()) }

	// before the opt-in nothing is mailed, and a call missed earlier is not mailed later
	missed(uid, "+49301111", time.Minute, false)
	if err := look(); err != nil || fm.count() != 0 {
		t.Fatalf("mail without an opt-in: %d %v", fm.count(), err)
	}
	u := e.userBrowser(t)
	if r := e.post(t, u, "/account/notifications", url.Values{"missed": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("opt in: %d", r.StatusCode)
	}
	if err := look(); err != nil || fm.count() != 0 {
		t.Fatalf("a call missed before the opt-in was mailed: %d %v", fm.count(), err)
	}

	// after it: two missed calls make one mail; an answered call, another user's call, a call that has just ended and
	// an old one make none
	missed(uid, "+49302222", time.Minute, false)
	missed(uid, "+49303333", time.Minute, false)
	missed(uid, "+49304444", time.Minute, true)
	missed(other, "+49305555", time.Minute, false) // the administrator has not opted in
	missed(uid, "+49306666", 5*time.Second, false)
	if err := look(); err != nil {
		t.Fatal(err)
	}
	m := fm.wait(t, 1)
	if fm.count() != 1 || m.to != "user@example.org" || m.subject != "2 missed calls" {
		t.Fatalf("mail: %d %+v", fm.count(), m)
	}
	for _, want := range []string{"+49302222", "+49303333", "line Home", "handset Kitchen", "All calls: ", "/calls?dir=missed"} {
		if !strings.Contains(m.body, want) {
			t.Errorf("the mail lacks %q: %s", want, m.body)
		}
	}
	for _, not := range []string{"+49301111", "+49304444", "+49305555", "+49306666"} {
		if strings.Contains(m.body, not) {
			t.Errorf("the mail has %q: %s", not, m.body)
		}
	}
	// once: a second look sends nothing, and the call that had just ended goes out when it has settled
	if err := look(); err != nil || fm.count() != 1 {
		t.Fatalf("a second look mailed again: %d %v", fm.count(), err)
	}
	if err := e.app.sendMissedCallMails(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if m := fm.wait(t, 2); m.subject != "Missed call from +49306666" {
		t.Errorf("second mail: %+v", m)
	}
	// switching it off stops it
	if r := e.post(t, u, "/account/notifications", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatal(r.StatusCode)
	}
	missed(uid, "+49307777", time.Minute, false)
	if err := look(); err != nil || fm.count() != 2 {
		t.Errorf("mail after the opt-out: %d %v", fm.count(), err)
	}
}
