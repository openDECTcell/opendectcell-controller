// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Message waiting. The SIP side learns from an account's mailbox how many new
// messages it holds (internal/sip, mwi.go); the bridge hands the count of each
// handset's line to SendMWI, which tells the cell the handset is on. The
// library keeps the count per record and shows it again after each location
// registration, so a handset that comes into a table or a stack that starts
// is told once by replayMWI (cell.go).

// MWILine is a line of a handset and the number of new messages in its mailbox.
type MWILine struct {
	Line  int
	Count uint
}

func mwiLine(line int, count uint) string {
	return fmt.Sprintf("mwi hs={hs} line=%d count=%d", line, count)
}

// SendMWI tells the cell a handset was last heard on that the mailbox of its
// line holds count new messages (0: none). A handset that is on no cell now is
// told when it comes to one (Core.MWIFor).
func (c *Core) SendMWI(hid uuid.UUID, line int, count uint) {
	cell := uuid.Nil
	if h, ok := c.lastHeard(hid); ok {
		cell = h.cell
	}
	if cell == uuid.Nil && c.Records != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cell, _ = c.Records.LastSeenCell(ctx, hid)
	}
	if cell == uuid.Nil {
		return
	}
	c.post(cell, event{kind: evCtl, hid: hid, line: mwiLine(line, count)})
}

// replayMWI tells the stack what the mailboxes of the handset's lines hold: the
// handset has just come into this cell's table, or the stack has just started.
func (r *cellRun) replayMWI(hid uuid.UUID) {
	if r.core.MWIFor == nil || r.st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
	lines := r.core.MWIFor(ctx, hid)
	cancel()
	for _, l := range lines {
		r.ctl(event{kind: evCtl, hid: hid, line: mwiLine(l.Line, l.Count)})
	}
}
