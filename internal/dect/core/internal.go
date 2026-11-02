// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"strconv"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// Internal calls (docs/internal-calls.md). A handset dials another handset by INT and a number; the
// stack hands the call to the core as `call-out class=int` (libopendectcell API 153) and the core finds the
// handset it means. The one rule that matters: a handset can only call the handsets of its own
// account. It is enforced here, where the number is resolved, and again where the call is made.
//
// The number a handset dials is one of three, all of which it learns from its own lists:
//   - 200 and up: the internal number of a handset of its account (the web interface shows it);
//   - stack.RemoteFirst..: an entry its INT list shows for a handset on another cell (directory.go);
//   - 1..32: the record number of a handset in this cell's table (what a Part 3 handset's list holds).

// maxSlotNumber is the largest record number of a cell's table.
const maxSlotNumber = 32

// firstInternal is the lowest internal number (migration 0018).
const firstInternal = 200

// resolveInternal is the handset that caller dialled with digits, uuid.Nil if there is none it may call.
func (r *cellRun) resolveInternal(caller uuid.UUID, digits string) uuid.UUID {
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 || caller == uuid.Nil || r.core.Records == nil {
		return uuid.Nil
	}
	ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
	defer cancel()
	_, _, owner, ok, err := r.core.Records.InternalIdentity(ctx, caller)
	if err != nil || !ok {
		return uuid.Nil
	}
	var target uuid.UUID
	switch {
	case n >= firstInternal:
		target, ok, err = r.core.Records.InternalByNumber(ctx, owner, n)
		if err != nil || !ok {
			return uuid.Nil
		}
	case n >= stack.RemoteFirst && n <= stack.RemoteLast:
		for hid, id := range r.dirIDs {
			if int(id) == n {
				target = hid
			}
		}
	case n <= maxSlotNumber && r.gen != nil:
		for i, slot := range r.gen.slotOf {
			if int(slot) == n {
				target = r.hidOf[i]
			}
		}
	}
	if target == uuid.Nil || target == caller {
		return uuid.Nil
	}
	// whichever way it was found, it is the caller's own account's
	_, _, to, ok, err := r.core.Records.InternalIdentity(ctx, target)
	if err != nil || !ok || to != owner {
		r.log.Info("an internal call to another account's handset is refused", "caller", caller)
		return uuid.Nil
	}
	return target
}

// InternalIdentity is what an internal call shows of a handset: its internal number as text and its name.
func (c *Core) InternalIdentity(ctx context.Context, hid uuid.UUID) (number, name string, owner uuid.UUID, ok bool, err error) {
	n, name, owner, ok, err := c.Records.InternalIdentity(ctx, hid)
	if err != nil || !ok {
		return "", "", uuid.Nil, false, err
	}
	if n > 0 {
		number = strconv.Itoa(n)
	}
	return number, name, owner, true, nil
}
