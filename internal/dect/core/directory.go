// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// The directory. A handset's Internal Names List (the INT key) shows the handsets of its user's
// account. The stack builds it from the records in its own table, so a handset on another cell would
// be missing: the user walks from one cell to the next and their other handsets vanish from the list.
// The core therefore gives each stack the account's handsets that are not in its table as the remote
// directory (libopendectcell API 152), each with a number of its own (stack.RemoteFirst..), and
// NameVisible answers for those numbers by the owner. An entry is a name only; a call to it goes
// through the controller (not built yet).

// DirHandset is a handset as the directory lists it.
type DirHandset struct {
	ID    uuid.UUID
	Owner uuid.UUID
	Name  string
}

// dirEvery (a variable for the tests) is the shortest time between two directory loads of a cell,
// dirRefresh how long a cell goes without one (names are edited in the web interface, which tells
// nobody).
var (
	dirEvery   = 2 * time.Second
	dirRefresh = time.Minute
)

// DirectoryChanged tells the cells that handsets of owner moved between tables or changed name
// (uuid.Nil: every cell). Only a cell that holds one of the owner's handsets lists the account's
// others, so only those read the directory again.
func (c *Core) DirectoryChanged(owner uuid.UUID) {
	c.mu.Lock()
	ids := make([]uuid.UUID, 0, len(c.cells))
	for id := range c.cells {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.post(id, event{kind: evDirectory, hid: owner})
	}
}

// holdsOwner says whether a handset of owner is in this cell's table.
func (r *cellRun) holdsOwner(owner uuid.UUID) bool {
	if r.gen == nil {
		return false
	}
	for _, o := range r.gen.owner {
		if o == owner {
			return true
		}
	}
	return false
}

// pushDirectory gives the stack the directory if it is due.
func (r *cellRun) pushDirectory(now time.Time) {
	if r.st == nil || r.gen == nil || r.core.Records == nil {
		return
	}
	if !r.dirDirty && now.Sub(r.dirAt) < dirRefresh {
		return
	}
	if now.Sub(r.dirAt) < dirEvery {
		return
	}
	r.dirDirty = false
	r.dirAt = now

	var owners, exclude []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for i, hid := range r.hidOf {
		exclude = append(exclude, hid)
		if o := r.gen.owner[r.gen.slotOf[i]]; o != uuid.Nil && !seen[o] {
			seen[o] = true
			owners = append(owners, o)
		}
	}
	var hs []DirHandset
	if len(owners) > 0 {
		ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
		var err error
		hs, err = r.core.Records.Directory(ctx, owners, exclude, stack.RemoteMax)
		cancel()
		if err != nil {
			r.log.Warn("the directory could not be read", "err", err)
			return
		}
	}
	r.setDirectory(hs)
}

// setDirectory numbers the handsets (one that stays keeps its number) and hands them to the stack.
func (r *cellRun) setDirectory(hs []DirHandset) {
	have := map[uuid.UUID]bool{}
	for _, h := range hs {
		have[h.ID] = true
	}
	for hid, n := range r.dirIDs {
		if !have[hid] {
			delete(r.dirIDs, hid)
			delete(r.gen.owner, n)
		}
	}
	if r.dirIDs == nil {
		r.dirIDs = map[uuid.UUID]uint{}
	}
	used := map[uint]bool{}
	for _, n := range r.dirIDs {
		used[n] = true
	}
	sort.SliceStable(hs, func(a, b int) bool { return hs[a].Name < hs[b].Name })
	entries := make([]stack.DirEntry, 0, len(hs))
	for _, h := range hs {
		n, ok := r.dirIDs[h.ID]
		if !ok {
			for n = stack.RemoteFirst; n <= stack.RemoteLast && used[n]; n++ {
			}
			if n > stack.RemoteLast {
				break
			}
			used[n] = true
			r.dirIDs[h.ID] = n
		}
		r.gen.owner[n] = h.Owner
		name := h.Name
		if name == "" {
			name = "DECT"
		}
		entries = append(entries, stack.DirEntry{ID: n, Name: name})
	}
	if !r.st.SetDirectory(entries) {
		r.log.Warn("the stack refused the directory", "entries", len(entries))
	}
}
