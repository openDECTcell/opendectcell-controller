// SPDX-License-Identifier: AGPL-3.0-or-later

package bridge

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/core"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
)

// Message waiting: an account's mailbox (sip.Config.OnMWI) says how many new
// messages it holds; every handset that has the account as a line is told, on
// that line (core.SendMWI), and a handset that comes to a cell later is told by
// the core asking MWIFor.

// MWISender tells a handset's cell the mailbox of one of its lines changed
// (*core.Core).
type MWISender interface {
	SendMWI(hid uuid.UUID, line int, count uint)
}

// AccountMWI is the SIP side's report of an account's mailbox.
func (m *Manager) AccountMWI(account uuid.UUID, s sip.MWI) {
	m.mu.Lock()
	if m.mwi == nil {
		m.mwi = map[uuid.UUID]uint{}
	}
	m.mwi[account] = s.New
	m.mu.Unlock()
	if m.MWI == nil || m.Lines == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recv, err := m.Lines.Receivers(ctx, account)
	if err != nil {
		m.log().Warn("the handsets of an account could not be read for its message count", "account", account, "err", err)
		return
	}
	for _, r := range recv {
		m.MWI.SendMWI(r.Handset, r.Line, s.New)
	}
}

// MWIFor is what the mailboxes of a handset's lines hold, for the lines whose
// account has reported (core.Core.MWIFor).
func (m *Manager) MWIFor(ctx context.Context, hid uuid.UUID) []core.MWILine {
	if m.Lines == nil {
		return nil
	}
	lines, err := m.Lines.HandsetLines(ctx, hid)
	if err != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.MWILine
	for _, l := range lines {
		if n, ok := m.mwi[l.Account]; ok {
			out = append(out, core.MWILine{Line: l.Line, Count: n})
		}
	}
	return out
}
