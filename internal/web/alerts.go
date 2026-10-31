// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

// CellOfflineAfter is how long an active cell may be offline before the
// admins who asked are told.
var CellOfflineAfter = 10 * time.Minute

type cellAlert struct {
	name         string
	offlineSince time.Time
	alerted      bool
}

// RunCellAlerts watches the cells' links and e-mails admins who opted in
// when an active cell has been offline for CellOfflineAfter, and again when
// it is back. State is in memory: after a restart the clock starts anew, so a
// restart does not flood anyone. It stops with ctx.
func (s *Server) RunCellAlerts(ctx context.Context) {
	if s.Cells == nil || s.CellLink == nil {
		return
	}
	state := map[uuid.UUID]*cellAlert{}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkCellAlerts(ctx, time.Now(), state)
		}
	}
}

// checkCellAlerts is one look at the cells; it returns nothing, it mails.
func (s *Server) checkCellAlerts(ctx context.Context, now time.Time, state map[uuid.UUID]*cellAlert) {
	cells, err := s.Cells.ListCells(ctx, &auth.User{Role: auth.RoleAdmin})
	if err != nil {
		s.Log.Error("cell alerts: list", "err", err)
		return
	}
	seen := map[uuid.UUID]bool{}
	for _, c := range cells {
		if c.State != "active" {
			continue
		}
		seen[c.ID] = true
		st := state[c.ID]
		if st == nil {
			st = &cellAlert{}
			state[c.ID] = st
		}
		st.name = c.Name
		if s.liveStatus(c.ID).Online {
			if st.alerted {
				s.alertAdmins(ctx, "Cell back online: "+c.Name, fmt.Sprintf("The cell %q is online again.", c.Name), "cell.alert_online", c.ID)
			}
			*st = cellAlert{name: c.Name}
			continue
		}
		if st.offlineSince.IsZero() {
			st.offlineSince = now
		}
		if !st.alerted && now.Sub(st.offlineSince) >= CellOfflineAfter {
			st.alerted = true
			s.alertAdmins(ctx, "Cell offline: "+c.Name, fmt.Sprintf("The cell %q has been offline since %s (more than %d minutes).",
				c.Name, st.offlineSince.Local().Format("2006-01-02 15:04"), int(CellOfflineAfter.Minutes())), "cell.alert_offline", c.ID)
		}
	}
	for id := range state {
		if !seen[id] {
			delete(state, id) // revoked, disabled or removed: no alert for it
		}
	}
}

func (s *Server) alertAdmins(ctx context.Context, subject, body, action string, cell uuid.UUID) {
	_ = audit.RecordAs(ctx, s.Pool, audit.System, audit.Entry{Action: action, ObjectType: "cell", ObjectID: cell.String()})
	if !s.mailOn() {
		return
	}
	rows, err := s.Pool.Query(ctx, "SELECT email FROM app_user WHERE notify_cell_alerts AND role = 'admin' AND NOT disabled AND coalesce(email, '') <> ''")
	if err != nil {
		s.Log.Error("cell alerts: recipients", "err", err)
		return
	}
	var to []string
	for rows.Next() {
		var e string
		if rows.Scan(&e) == nil {
			to = append(to, e)
		}
	}
	rows.Close()
	if len(to) == 0 {
		return
	}
	if err := s.Mailer.Send(ctx, to, subject, body+"\n\nYou get this because you asked for cell alerts on your account page.\n"); err != nil {
		s.Log.Error("cell alerts: send", "err", err)
	}
}
