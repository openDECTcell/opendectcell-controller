// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
)

// onlineLink is a CellLink whose cells' state a test sets.
type onlineLink struct {
	mu sync.Mutex
	up map[uuid.UUID]bool
}

func (l *onlineLink) CellStatus(id uuid.UUID) cell.Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cell.Status{Online: l.up[id], Since: time.Now().Add(-time.Hour)}
}
func (l *onlineLink) NotifyState(uuid.UUID, wire.CellStateValue, string) {}
func (l *onlineLink) Kick(uuid.UUID)                                     {}
func (l *onlineLink) set(id uuid.UUID, up bool)                          { l.mu.Lock(); l.up[id] = up; l.mu.Unlock() }

func newOpsEnv(t *testing.T) (*env, *onlineLink, *fakeMail) {
	link := &onlineLink{up: map[uuid.UUID]bool{}}
	fm := &fakeMail{}
	e := newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Cells, err = cell.Open(ctx, s.Pool, kr, "dect.example.org:7300"); err != nil {
			t.Fatal(err)
		}
		s.CellLink, s.Mailer = link, fm
		s.MetricsToken = "s3cret-token"
		s.StartedAt = time.Now()
	})
	return e, link, fm
}

func TestCellOfflineAlerts(t *testing.T) {
	e, link, fm := newOpsEnv(t)
	var cid uuid.UUID
	if err := e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('Attic', 'active') RETURNING id").Scan(&cid); err != nil {
		t.Fatal(err)
	}
	e.store.Pool.Exec(ctx, "UPDATE app_user SET email = 'admin@example.org' WHERE username = 'admin'")
	e.store.Pool.Exec(ctx, "UPDATE app_user SET email = 'user@example.org', notify_cell_alerts = true WHERE username = 'user'") // a user is never mailed
	// opting in: only admins can
	a := e.adminBrowser(t)
	if r := e.post(t, a, "/account/notifications", url.Values{"cells": {"on"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("opt in: %d", r.StatusCode)
	}
	srv := e.app
	state := map[uuid.UUID]*cellAlert{}
	now := time.Now()
	check := func(at time.Time) { srv.checkCellAlerts(context.Background(), at, state) }

	check(now) // first look: offline, the clock starts
	check(now.Add(CellOfflineAfter - time.Minute))
	time.Sleep(50 * time.Millisecond)
	if fm.count() != 0 {
		t.Fatal("alert before the time")
	}
	check(now.Add(CellOfflineAfter + time.Minute))
	m := fm.wait(t, 1)
	if m.to != "admin@example.org" || !strings.Contains(m.subject, "offline") || !strings.Contains(m.body, "Attic") {
		t.Fatalf("alert mail: %+v", m)
	}
	check(now.Add(CellOfflineAfter + 5*time.Minute)) // no second alert
	time.Sleep(50 * time.Millisecond)
	if fm.count() != 1 {
		t.Errorf("repeated alert: %d mails", fm.count())
	}
	link.set(cid, true)
	check(now.Add(CellOfflineAfter + 6*time.Minute))
	if m := fm.wait(t, 2); !strings.Contains(m.subject, "back online") {
		t.Errorf("recovery mail: %+v", m)
	}
	// a flap that never reached the threshold says nothing
	link.set(cid, false)
	check(now.Add(time.Hour))
	link.set(cid, true)
	check(now.Add(time.Hour + time.Minute))
	time.Sleep(50 * time.Millisecond)
	if fm.count() != 2 {
		t.Errorf("a short outage was announced: %d mails", fm.count())
	}
	var n int
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action IN ('cell.alert_offline', 'cell.alert_online')").Scan(&n)
	if n != 2 {
		t.Errorf("alerts audited: %d", n)
	}
	// users cannot opt in to cell alerts
	u := e.userBrowser(t)
	e.post(t, u, "/account/notifications", url.Values{"cells": {"on"}, "log": {"on"}})
	var on bool
	e.store.Pool.QueryRow(ctx, "SELECT notify_cell_alerts FROM app_user WHERE username = 'user'").Scan(&on)
	if on {
		t.Error("a user opted in to cell alerts")
	}
}

func TestMetrics(t *testing.T) {
	e, link, _ := newOpsEnv(t)
	var cid uuid.UUID
	e.store.Pool.QueryRow(ctx, `INSERT INTO cell (name, state) VALUES ('At"tic', 'active') RETURNING id`).Scan(&cid)
	link.set(cid, true)
	c := e.browser()
	if resp, _ := e.get(t, c, "/metrics"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	if resp, _ := c.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer s3cret-token")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(resp)
	for _, want := range []string{"opendectcell_build_info{version=\"test\"} 1", "opendectcell_cells{state=\"active\"} 1", "opendectcell_cell_up{cell=\"At\\\"tic\"} 1", "opendectcell_cells_online 1", "opendectcell_users 2", "# TYPE opendectcell_calls_total counter"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
	// off without a token
	off := newEnv(t)
	if resp, _ := off.get(t, off.browser(), "/metrics"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("metrics without a configured token: %d", resp.StatusCode)
	}
}

func TestRetentionSettingsAndBackupPage(t *testing.T) {
	e := newEnv(t)
	a := e.adminBrowser(t)
	if r := e.post(t, a, "/admin/settings/retention", url.Values{"call_detail_days": {"90"}, "audit_days": {"365"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", r.StatusCode)
	}
	if _, body := e.get(t, a, "/admin/settings"); !strings.Contains(body, `value="90"`) || !strings.Contains(body, `value="365"`) {
		t.Error("saved retention not shown")
	}
	for _, bad := range []string{"3", "abc", "-5", "99999"} {
		if r := e.post(t, a, "/admin/settings/retention", url.Values{"call_detail_days": {bad}}); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%q accepted: %d", bad, r.StatusCode)
		}
	}
	if r := e.post(t, a, "/admin/settings/retention", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Errorf("clearing: %d", r.StatusCode)
	}
	if r := e.post(t, e.userBrowser(t), "/admin/settings/retention", url.Values{"call_detail_days": {"30"}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("a user set retention: %d", r.StatusCode)
	}
	if resp, body := e.get(t, a, "/admin/backup"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "master key") {
		t.Errorf("backup page: %d", resp.StatusCode)
	}
}
