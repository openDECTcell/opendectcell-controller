// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

// Handset is one record a cell serves: the handset and where it sits in
// that cell's chip.
type Handset struct {
	ID    uuid.UUID
	Owner uuid.UUID // always set: there is no ownerless handset (the maintainers' notes)
	Name  string
	// Profile is the id of its device profile (internal/dect/profiles); "" is
	// the standard behaviour.
	Profile string
	Rec     stack.Record // Rec.Slot is the cell's chip slot
}

// Records is where the core keeps handset records (Store in production).
type Records interface {
	// CellRecords is what cell holds, at most limit records, placed by
	// cell-protocol.md §7.4 rules 1 and 2 (home set, no eviction). unplaced
	// lists home-set handsets that found no free slot.
	CellRecords(ctx context.Context, cell uuid.UUID, limit int) (hs []Handset, unplaced []uuid.UUID, err error)
	// SaveRecord stores a record the stack changed. id uuid.Nil is a
	// handset that just enrolled on cell through owner's registration
	// window; its id is returned. owner is ignored for a known id.
	SaveRecord(ctx context.Context, cell, id, owner uuid.UUID, r stack.Record) (Saved, error)
	// PlaceHandset puts a handset into cell's table for as long as it is
	// heard there: the lowest free record number above the chip's slots (a
	// handset already placed there comes back as it is). ok is false for a
	// handset that does not exist; ErrCellFull when no number is free.
	// PlaceByIPUI is the same for the handset with that IPUI.
	PlaceHandset(ctx context.Context, cell, id uuid.UUID) (h Handset, ok bool, err error)
	PlaceByIPUI(ctx context.Context, cell uuid.UUID, ipui []byte) (h Handset, ok bool, err error)
	// Unplace: the handset left cell's table; its record number is free.
	Unplace(ctx context.Context, cell, id uuid.UUID) error
	// ReleaseChipSlot: the handset's record left one of cell's chip slots (a
	// pooled handset enrolled into it and was recycled out): the slot is free.
	ReleaseChipSlot(ctx context.Context, cell, id uuid.UUID) error
	// HandsetKnown: there is such a handset.
	HandsetKnown(ctx context.Context, id uuid.UUID) (bool, error)
	// ReserveTPUI takes a TPUI from the pool for an enrolment cell may
	// take, one no handset in that cell's table holds; all zero when the pool
	// is off. ReleaseTPUI gives it back unused.
	ReserveTPUI(ctx context.Context, cell uuid.UUID) ([3]byte, error)
	ReleaseTPUI(cell uuid.UUID, t [3]byte)
	// Forget: a handset deregistered on cell; it is no longer placed there.
	// A pooled handset is deregistered everywhere (pooled says so, and the
	// other cells must drop it).
	Forget(ctx context.Context, cell, id uuid.UUID) (pooled bool, err error)
	// Rename sets a handset's name.
	Rename(ctx context.Context, id uuid.UUID, name string) error
	// SetCapabilities keeps what a handset said of itself, for its page.
	SetCapabilities(ctx context.Context, id uuid.UUID, caps map[string]any) error
	// SetSignal keeps the last field strength measured on a link to the handset.
	SetSignal(ctx context.Context, id uuid.UUID, dbm int, at time.Time) error
	// HandsetInfo is what the stacks show of a handset: its name and the id of
	// its device profile ("" for none). ok is false for a handset that is gone.
	HandsetInfo(ctx context.Context, id uuid.UUID) (name, profile string, ok bool, err error)
	// Installed: the cell's chip was written: the slots listed hold their
	// handsets, every other slot of the cell is empty.
	Installed(ctx context.Context, cell uuid.UUID, slots []uint) error
	// SlotUnknown: a write to slot was not delivered.
	SlotUnknown(ctx context.Context, cell uuid.UUID, slot uint) error
	// LastSeenCell is the cell the handset was last heard on, uuid.Nil if
	// none.
	LastSeenCell(ctx context.Context, id uuid.UUID) (uuid.UUID, error)
	// CellOrder ranks cells (the ones that are up) for a call to the handset:
	// those it was heard on in the last week, the most recent first, then the
	// nearest to anchor (the cell it was last on; uuid.Nil: none), cells with
	// no position last; at most limit.
	CellOrder(ctx context.Context, id, anchor uuid.UUID, cells []uuid.UUID, limit int) ([]uuid.UUID, error)
	// InternalByNumber is the handset of owner that has internal number n (docs/internal-calls.md).
	InternalByNumber(ctx context.Context, owner uuid.UUID, n int) (id uuid.UUID, ok bool, err error)
	// InternalIdentity is what a handset shows of itself in an internal call: its internal number (0:
	// it has none), its name, and its owner. ok is false for a handset that is gone.
	InternalIdentity(ctx context.Context, id uuid.UUID) (number int, name string, owner uuid.UUID, ok bool, err error)
	// Detached: the handset sent {DETACH} (it is switched off) at the given time. PageFailed:
	// nobody answered a page for it. Both only say what is shown of the handset (presence).
	Detached(ctx context.Context, id uuid.UUID, at time.Time) error
	PageFailed(ctx context.Context, id uuid.UUID, at time.Time) error
	// Directory lists the handsets of these owners that are not among exclude, by name, at most limit:
	// what the Internal Names List shows of handsets on other cells.
	Directory(ctx context.Context, owners, exclude []uuid.UUID, limit int) ([]DirHandset, error)
	// Seen: the handset had contact on cell at the given time. It returns the
	// cell it was last seen on before, uuid.Nil if none, so a move between
	// cells can be told.
	Seen(ctx context.Context, cell, id uuid.UUID, at time.Time) (prev uuid.UUID, err error)
}

// Store keeps the records in PostgreSQL, the UAK sealed (AGENTS.md §5).
type Store struct {
	Pool    *pgxpool.Pool
	Keyring *secret.Keyring
	// NoPool: handsets that enrol are chip-slot handsets of their cell
	// (config cells.handset_pool off). Pooled handsets that exist are still
	// held on every cell.
	NoPool bool

	mu       sync.Mutex
	reserved map[uuid.UUID]map[uint8]bool // pool TPUIs offered to a free record of a cell, by low byte
}

// Saved is what SaveRecord did.
type Saved struct {
	ID uuid.UUID
	// Enrolled: the save was an enrolment (the stack's first save of the
	// record); Again: of a handset that was known by its IPUI.
	Enrolled, Again bool
	// Pooled: the handset is held by every cell.
	Pooled bool
}

var _ Records = (*Store)(nil)

func uakLoc(id uuid.UUID) secret.Location {
	return secret.Location{Table: "handset", Column: "uak_enc", RowID: id.String()}
}

// maxSlot is the chip's subscription table (DECT_MM_MAX_SLOT); maxRecord is
// how many records a stack holds (DECT_HS_MAX, 32): the record numbers above
// maxSlot belong to the host alone (libopendectcell API 144).
const (
	maxSlot   = 6
	maxRecord = 32
	poolFirst = maxSlot + 1 // the first record number above the chip's slots
)

// poolValid says whether N may be the last byte of a pool TPUI {00, 11, N}. The
// 06.88 chip routes a handset's own access (a call, the answer to a page) by
// the low three bits of that byte and hears only 1 to 6; a byte with bit 3 set
// is not heard either (bench 2026-11-02: 02, 16, 25, 35, 42, 43, f1 answered;
// 0d, 17, 19, 3d never), while the high nibble is free. Below 0x10 are the
// chip's own slot TPUIs. The 08.25 chip hears any.
func poolValid(n int) bool {
	lo := n & 15
	return n >= 0x10 && n <= 0xFF && lo >= 1 && lo <= 6
}

// poolSize is how many TPUIs the pool holds: 15 high nibbles by 6 classes. A
// TPUI must be unique among the handsets in one cell's table (the chip pages
// and hears by it), not across the controller: a cell holds 32 records, so the
// pool is a per-cell limit and the number of handsets is not limited by it.
const poolSize = 90

// poolTPUI is a pool TPUI {00, 11, N}, N one poolValid allows, as the DB column holds it.
func poolTPUI(t [3]byte) (int32, bool) {
	if t[0] != 0 || t[1] != 0x11 || !poolValid(int(t[2])) {
		return 0, false
	}
	return 0x1100 | int32(t[2]), true
}

func tpuiBytes(v int32) [3]byte { return [3]byte{byte(v >> 16), byte(v >> 8), byte(v)} }

// ErrPoolFull means every TPUI of the pool is held in this cell's table (poolSize
// handsets). The limit is per cell: the pool is not shared across cells.
var ErrPoolFull = errors.New("core: the TPUI pool of the cell is full (90 handsets)")

// cellTPUIs is the low bytes of the pool TPUIs the pooled handsets in cell's
// table hold.
func cellTPUIs(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, cell uuid.UUID) (map[int]bool, error) {
	rows, err := q.Query(ctx, `SELECT (h.tpui & 255)::int FROM handset h JOIN cell_slot cs ON cs.handset_id = h.id
		WHERE cs.cell_id = $1 AND h.pooled AND h.tpui & 65280 = 4352`, cell)
	if err != nil {
		return nil, err
	}
	taken, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, t := range taken {
		used[t] = true
	}
	return used, nil
}

// pickPoolByte is a random pool byte not in used, or false.
func pickPoolByte(used map[int]bool) (uint8, bool, error) {
	var free []uint8
	for n := 0x10; n <= 255; n++ {
		if poolValid(n) && !used[n] {
			free = append(free, uint8(n))
		}
	}
	if len(free) == 0 {
		return 0, false, nil
	}
	k, err := rand.Int(rand.Reader, big.NewInt(int64(len(free))))
	if err != nil {
		return 0, false, err
	}
	return free[k.Int64()], true, nil
}

// ReserveTPUI picks a pool TPUI that no pooled handset in cell's table holds
// and no other reservation of that cell has taken, and remembers it until
// ReleaseTPUI or the enrolment's SaveRecord. It returns all zero when the pool is
// off (NoPool) and ErrPoolFull when none is free. Reservations are kept in memory.
func (s *Store) ReserveTPUI(ctx context.Context, cell uuid.UUID) ([3]byte, error) {
	if s.NoPool {
		return [3]byte{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	used, err := cellTPUIs(ctx, s.Pool, cell)
	if err != nil {
		return [3]byte{}, err
	}
	for n := range s.reserved[cell] {
		used[int(n)] = true
	}
	n, ok, err := pickPoolByte(used)
	if err != nil {
		return [3]byte{}, err
	}
	if !ok {
		return [3]byte{}, ErrPoolFull
	}
	if s.reserved == nil {
		s.reserved = map[uuid.UUID]map[uint8]bool{}
	}
	if s.reserved[cell] == nil {
		s.reserved[cell] = map[uint8]bool{}
	}
	s.reserved[cell][n] = true
	return [3]byte{0, 0x11, n}, nil
}

// ReleaseTPUI gives back a TPUI that ReserveTPUI offered and nobody used. A TPUI
// that is not a pool one is ignored.
func (s *Store) ReleaseTPUI(cell uuid.UUID, t [3]byte) {
	if t[1] != 0x11 {
		return
	}
	s.mu.Lock()
	delete(s.reserved[cell], t[2])
	s.mu.Unlock()
}

// uniqueTPUIs gives every pooled handset in cell's table a TPUI of its own
// there: the chip pages a handset by its TPUI and would ring every handset
// that holds the same. The TPUI is unique per cell, not across the controller
// (a handset takes the one the cell it locates on gives it). Of two that
// clash the older keeps it, newcomer (if any) last; the new TPUI reaches the
// handset at its next location registration.
func (s *Store) uniqueTPUIs(ctx context.Context, tx pgx.Tx, cell, newcomer uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT h.id, h.tpui FROM handset h JOIN cell_slot cs ON cs.handset_id = h.id
		WHERE cs.cell_id = $1 AND h.pooled ORDER BY (h.id = $2), h.registered_at, h.id`, cell, newcomer)
	if err != nil {
		return err
	}
	type held struct {
		id   uuid.UUID
		tpui int32
	}
	hs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (held, error) {
		var h held
		return h, r.Scan(&h.id, &h.tpui)
	})
	if err != nil {
		return err
	}
	used := map[int]bool{}
	s.mu.Lock()
	for n := range s.reserved[cell] {
		used[int(n)] = true
	}
	s.mu.Unlock()
	for _, h := range hs {
		if h.tpui&0xFF00 == 0x1100 {
			used[int(h.tpui&0xFF)] = true
		}
	}
	seen := map[int32]bool{}
	for _, h := range hs {
		v := h.tpui & 0xFFFF
		if !seen[v] {
			seen[v] = true
			continue
		}
		n, ok, err := pickPoolByte(used)
		if err != nil {
			return err
		}
		if !ok {
			continue // every TPUI is taken in this cell: it keeps the clash
		}
		used[int(n)] = true
		seen[0x1100|int32(n)] = true
		if _, err := tx.Exec(ctx, "UPDATE handset SET tpui = $2 WHERE id = $1", h.id, 0x1100|int32(n)); err != nil {
			return err
		}
	}
	return nil
}

// CellRecords loads what cell holds for its stack. It places the home set
// that has no slot yet into the lowest free chip slot, up to limit (never more
// than maxSlot), empties the host-only records that a stack does not keep across
// a start, and makes the pooled TPUIs unique. The handsets that found no slot
// are returned as unplaced. The UAKs in the result are decrypted.
// One placement per cell runs at a time (an advisory lock).
func (s *Store) CellRecords(ctx context.Context, cell uuid.UUID, limit int) ([]Handset, []uuid.UUID, error) {
	limit = min(limit, maxSlot)
	var hs []Handset
	var unplaced []uuid.UUID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		hs, unplaced = nil, nil
		// One placement at a time per cell.
		if err := lockCell(ctx, tx, cell); err != nil {
			return err
		}
		// Home-set handsets without a slot on this cell get the lowest free one.
		rows, err := tx.Query(ctx, `SELECT hc.handset_id FROM handset_home_cell hc JOIN handset h ON h.id = hc.handset_id
			WHERE hc.cell_id = $1 AND NOT h.pooled AND handset_may_use_cell(h.owner_user_id, hc.cell_id) AND NOT EXISTS
				(SELECT 1 FROM cell_slot cs WHERE cs.cell_id = $1 AND cs.handset_id = hc.handset_id)
			ORDER BY h.registered_at, h.id`, cell)
		if err != nil {
			return err
		}
		homeless, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		used, err := usedSlots(ctx, tx, cell)
		if err != nil {
			return err
		}
		for _, id := range homeless {
			slot := int16(1)
			for used[slot] && slot <= int16(limit) {
				slot++
			}
			if slot > int16(limit) {
				unplaced = append(unplaced, id)
				continue
			}
			used[slot] = true
			if err := putSlot(ctx, tx, cell, slot, id, false); err != nil {
				return err
			}
		}
		// Above the chip's slots the table is a working set that a stack starts
		// without: whoever was in it before has to be heard (or called) to come
		// back (Fetch, Ring). A pooled handset that sits in a chip slot here (it
		// enrolled in this run) gives the slot up the same way.
		if _, err := tx.Exec(ctx, `UPDATE cell_slot cs SET handset_id = NULL, state = 'empty', written_at = now()
			WHERE cs.cell_id = $1 AND cs.handset_id IS NOT NULL AND (cs.slot > $2 OR EXISTS
				(SELECT 1 FROM handset h WHERE h.id = cs.handset_id AND h.pooled))`,
			cell, int16(maxSlot)); err != nil {
			return err
		}
		if err := s.uniqueTPUIs(ctx, tx, cell, uuid.Nil); err != nil {
			return err
		}
		hs, err = s.readRecords(ctx, tx, `WHERE cs.cell_id = $1 AND cs.slot BETWEEN 1 AND $2 ORDER BY cs.slot`, cell, int16(maxSlot))
		return err
	})
	if err != nil {
		for i := range hs {
			clear(hs[i].Rec.UAK[:])
		}
		return nil, nil, err
	}
	return hs, unplaced, nil
}

// ErrCellFull means every record number above the chip's slots holds a handset.
var ErrCellFull = errors.New("core: the cell's table is full")

// PlaceHandset puts a handset in cell's table to page it there: it has not been
// heard on the cell, so its TPUI is the one it holds and is left alone.
func (s *Store) PlaceHandset(ctx context.Context, cell, id uuid.UUID) (Handset, bool, error) {
	return s.place(ctx, cell, false, func(tx pgx.Tx) (uuid.UUID, error) {
		var got uuid.UUID
		err := tx.QueryRow(ctx, "SELECT id FROM handset WHERE id = $1", id).Scan(&got)
		return got, err
	})
}

// PlaceByIPUI puts a handset the cell has just heard in its table; if another
// handset there holds its TPUI it gets a new one, which the cell's answer to
// its location registration carries (uniqueTPUIs).
func (s *Store) PlaceByIPUI(ctx context.Context, cell uuid.UUID, ipui []byte) (Handset, bool, error) {
	return s.place(ctx, cell, true, func(tx pgx.Tx) (uuid.UUID, error) {
		var got uuid.UUID
		err := tx.QueryRow(ctx, "SELECT id FROM handset WHERE ipui = $1", ipui).Scan(&got)
		return got, err
	})
}

// place finds the handset, gives it a record number on cell if it has none
// there, and reads its record. heard: the handset talks to the cell now.
func (s *Store) place(ctx context.Context, cell uuid.UUID, heard bool, find func(pgx.Tx) (uuid.UUID, error)) (Handset, bool, error) {
	var out []Handset
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		out = nil
		if err := lockCell(ctx, tx, cell); err != nil {
			return err
		}
		id, err := find(tx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// The owner decides whose base stations may hold the handset's key
		// (docs/adr/0007-organizations-and-trust.md): a cell they do not trust gets no record.
		var allowed bool
		if err := tx.QueryRow(ctx, "SELECT handset_may_use_cell(owner_user_id, $2) FROM handset WHERE id = $1", id, cell).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return nil
		}
		var here bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM cell_slot WHERE cell_id = $1 AND handset_id = $2)", cell, id).Scan(&here); err != nil {
			return err
		}
		if !here {
			used, err := usedSlots(ctx, tx, cell)
			if err != nil {
				return err
			}
			slot := freeHostSlot(used)
			if slot == 0 {
				return ErrCellFull
			}
			if err := putSlot(ctx, tx, cell, slot, id, true); err != nil {
				return err
			}
		}
		if heard {
			if err := s.uniqueTPUIs(ctx, tx, cell, id); err != nil {
				return err
			}
		}
		out, err = s.readRecords(ctx, tx, `WHERE cs.cell_id = $1 AND cs.handset_id = $2`, cell, id)
		return err
	})
	if err != nil || len(out) != 1 {
		for i := range out {
			clear(out[i].Rec.UAK[:])
		}
		return Handset{}, false, err
	}
	return out[0], true, nil
}

// CellOrder ranks cells for a call to handset id: first those it was heard on in
// the last seven days (the moves that Seen records), the latest first, then by squared distance from anchor's
// position (an equirectangular approximation, enough to rank cells of one site),
// cells without a position last, ties by id; at most limit.
func (s *Store) CellOrder(ctx context.Context, id, anchor uuid.UUID, cells []uuid.UUID, limit int) ([]uuid.UUID, error) {
	var a any // a nil uuid is no anchor
	if anchor != uuid.Nil {
		a = anchor
	}
	rows, err := s.Pool.Query(ctx, `SELECT c.id FROM cell c
		LEFT JOIN (SELECT to_cell_id, max(at) AS at FROM handset_move
			WHERE handset_id = $1 AND at > now() - interval '7 days' GROUP BY to_cell_id) h ON h.to_cell_id = c.id
		LEFT JOIN cell a ON a.id = $3
		WHERE c.id = ANY($2) AND handset_may_use_cell((SELECT owner_user_id FROM handset WHERE id = $1), c.id)
		ORDER BY (h.at IS NULL), h.at DESC,
			(c.latitude IS NULL OR a.latitude IS NULL),
			(c.latitude - a.latitude)^2 + (cos(radians((c.latitude + a.latitude) / 2)) * (c.longitude - a.longitude))^2,
			c.id
		LIMIT $4`, id, cells, a, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// Unplace frees the host-only record number (above the chip's slots) that the
// handset held in cell's table.
func (s *Store) Unplace(ctx context.Context, cell, id uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `UPDATE cell_slot SET handset_id = NULL, state = 'empty', written_at = now()
		WHERE cell_id = $1 AND handset_id = $2 AND slot > $3`, cell, id, int16(maxSlot))
	return err
}

// ReleaseChipSlot frees the chip slot the handset held in cell. The row stays and
// is empty, so the next Installed or CellRecords can reuse it.
func (s *Store) ReleaseChipSlot(ctx context.Context, cell, id uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `UPDATE cell_slot SET handset_id = NULL, state = 'empty', written_at = now()
		WHERE cell_id = $1 AND handset_id = $2 AND slot <= $3`, cell, id, int16(maxSlot))
	return err
}

// HandsetKnown reports whether a handset with this id exists.
func (s *Store) HandsetKnown(ctx context.Context, id uuid.UUID) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM handset WHERE id = $1)", id).Scan(&ok)
	return ok, err
}

func lockCell(ctx context.Context, tx pgx.Tx, cell uuid.UUID) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('cell_slot'), hashtext($1))", cell.String())
	return err
}

// usedSlots is the record numbers that hold a handset on cell.
func usedSlots(ctx context.Context, tx pgx.Tx, cell uuid.UUID) (map[int16]bool, error) {
	rows, err := tx.Query(ctx, "SELECT slot FROM cell_slot WHERE cell_id = $1 AND handset_id IS NOT NULL", cell)
	if err != nil {
		return nil, err
	}
	taken, err := pgx.CollectRows(rows, pgx.RowTo[int16])
	if err != nil {
		return nil, err
	}
	used := map[int16]bool{}
	for _, t := range taken {
		used[t] = true
	}
	return used, nil
}

// freeHostSlot is the lowest record number above the chip's slots not in
// used; 0 if there is none.
func freeHostSlot(used map[int16]bool) int16 {
	for slot := int16(maxSlot + 1); slot <= maxRecord; slot++ {
		if !used[slot] {
			return slot
		}
	}
	return 0
}

// putSlot places id in record number slot of cell: a chip slot is unknown
// until written, a host-only record has nothing in the chip.
func putSlot(ctx context.Context, tx pgx.Tx, cell uuid.UUID, slot int16, id uuid.UUID, hostOnly bool) error {
	st := "unknown"
	if hostOnly {
		st = "empty"
	}
	_, err := tx.Exec(ctx, `INSERT INTO cell_slot (cell_id, slot, handset_id, state) VALUES ($1, $2, $3, $4)
		ON CONFLICT (cell_id, slot) DO UPDATE SET handset_id = EXCLUDED.handset_id, state = EXCLUDED.state`,
		cell, slot, id, st)
	return err
}

// readRecords reads the handsets of the cell_slot rows a WHERE clause picks
// (cs is cell_slot, h is handset), the UAK opened.
func (s *Store) readRecords(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]Handset, error) {
	rows, err := tx.Query(ctx, `SELECT h.id, h.owner_user_id, h.display_name, cs.slot, h.ipui, h.uak_enc,
			coalesce(h.cc_port_id, ''::bytea), coalesce(h.cc_fixed_id, ''::bytea), h.pooled, h.tpui, coalesce(h.profile, '')
		FROM cell_slot cs JOIN handset h ON h.id = cs.handset_id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hs []Handset
	fail := func(err error) ([]Handset, error) {
		for i := range hs {
			clear(hs[i].Rec.UAK[:])
		}
		return nil, err
	}
	for rows.Next() {
		var h Handset
		var slot int16
		var enc []byte
		var pooled bool
		var tpui int32
		if err := rows.Scan(&h.ID, &h.Owner, &h.Name, &slot, &h.Rec.PortID, &enc,
			&h.Rec.CCPortID, &h.Rec.CCFixedID, &pooled, &tpui, &h.Profile); err != nil {
			return fail(err)
		}
		uak, err := s.Keyring.Open(uakLoc(h.ID), enc)
		if err != nil {
			return fail(fmt.Errorf("handset %s: UAK: %w", h.ID, err))
		}
		if len(uak) != len(h.Rec.UAK) {
			clear(uak)
			return fail(fmt.Errorf("handset %s: UAK of %d bytes", h.ID, len(uak)))
		}
		copy(h.Rec.UAK[:], uak)
		clear(uak)
		h.Rec.Slot = uint(slot)
		if pooled {
			h.Rec.TPUI = tpuiBytes(tpui)
		}
		hs = append(hs, h)
	}
	if err := rows.Err(); err != nil {
		return fail(err)
	}
	return hs, nil
}

// errNoOwner: an enrolment came without a registration window's owner.
var errNoOwner = errors.New("core: an enrolment without an owner (no registration window)")

// errOtherOwner: a handset enrolled again through another user's window.
var errOtherOwner = errors.New("core: a handset of another owner enrolled again")

// errTPUI means no free TPUI was found; it does not happen in practice.
var errTPUI = errors.New("core: no free TPUI found")

// newTPUI picks a TPUI for a chip-slot handset whose low 16 bits, the link
// signature, are unused too (cell-protocol.md §7.4), and out of the pool's
// range 11 00 to 11 FF.
func newTPUI(ctx context.Context, tx pgx.Tx) (int32, error) {
	var b [4]byte
	for range 32 {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		t := int32(binary.BigEndian.Uint32(b[:]) & 0xFFFFF)
		var used bool
		if t&0xFF00 == 0x1100 {
			continue
		}
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM handset WHERE tpui & 65535 = $1)", t&0xFFFF).Scan(&used); err != nil {
			return 0, err
		}
		if !used {
			return t, nil
		}
	}
	return 0, errTPUI
}

func nullIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// SaveRecord stores a record the stack changed, in one transaction: the handset
// row (key sealed with the keyring, identity, call-control identifiers) and the
// cell slot that holds it. With id uuid.Nil it is an enrolment: a new row is
// created for owner, or, for an IPUI that is already known, its key is replaced;
// a known IPUI of another owner is an error, because a change of owner is not built.
// Record numbers outside 1 to maxRecord are refused.
func (s *Store) SaveRecord(ctx context.Context, cell, id, owner uuid.UUID, r stack.Record) (Saved, error) {
	if r.Slot < 1 || r.Slot > maxRecord {
		return Saved{}, fmt.Errorf("core: record number %d", r.Slot)
	}
	if id == uuid.Nil && owner == uuid.Nil {
		return Saved{}, errNoOwner
	}
	var out Saved
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		out = Saved{ID: id}
		if id == uuid.Nil {
			if len(r.PortID) == 0 {
				return errors.New("core: an enrolled record without an identity")
			}
			out.Enrolled = true
			// A pool TPUI on an enrolment is the one the cell was given to
			// offer (ReserveTPUI): unique in that cell's table; any other
			// cell the handset locates on makes it unique there (uniqueTPUIs).
			pt, pooled := poolTPUI(r.TPUI)
			pooled = pooled && !s.NoPool
			// A handset enrolling again keeps its row: same IPUI, new key.
			// Only its owner can do that; a change of owner is not built.
			var was uuid.UUID
			err := tx.QueryRow(ctx, "SELECT id, owner_user_id FROM handset WHERE ipui = $1", r.PortID).Scan(&id, &was)
			if err == nil && was != owner {
				return errOtherOwner
			}
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				id = uuid.New()
				tpui := pt
				if !pooled {
					if tpui, err = newTPUI(ctx, tx); err != nil {
						return err
					}
				}
				enc, err := s.Keyring.Seal(uakLoc(id), r.UAK[:])
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO handset (id, owner_user_id, ipui, tpui, uak_enc, cc_port_id, cc_fixed_id, pooled)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, owner, r.PortID, tpui, enc,
					nullIfEmpty(r.CCPortID), nullIfEmpty(r.CCFixedID), pooled); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO handset_home_cell (handset_id, cell_id) VALUES ($1, $2)
					ON CONFLICT DO NOTHING`, id, cell); err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				out.Again = true
				if pooled {
					// It holds the TPUI this enrolment assigned from now on.
					if _, err := tx.Exec(ctx, "UPDATE handset SET tpui = $2, pooled = true WHERE id = $1", id, pt); err != nil {
						return err
					}
				}
			}
			out.ID = id
		}
		if err := tx.QueryRow(ctx, "SELECT pooled FROM handset WHERE id = $1", id).Scan(&out.Pooled); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("core: handset %s is gone", id)
			}
			return err
		}
		enc, err := s.Keyring.Seal(uakLoc(id), r.UAK[:])
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE handset SET uak_enc = $2, ipui = coalesce($3, ipui),
				cc_port_id = coalesce($4, cc_port_id), cc_fixed_id = coalesce($5, cc_fixed_id)
			WHERE id = $1`, id, enc, nullIfEmpty(r.PortID), nullIfEmpty(r.CCPortID), nullIfEmpty(r.CCFixedID))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("core: handset %s is gone", id)
		}
		// The handset is in this slot of this cell, and nowhere else on it.
		if _, err := tx.Exec(ctx, `UPDATE cell_slot SET handset_id = NULL, state = 'unknown'
			WHERE cell_id = $1 AND handset_id = $2 AND slot <> $3`, cell, id, int16(r.Slot)); err != nil {
			return err
		}
		state := "present"
		if r.Slot > maxSlot {
			state = "empty" // host-only: nothing in the chip
		}
		_, err = tx.Exec(ctx, `INSERT INTO cell_slot (cell_id, slot, handset_id, state, written_at) VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (cell_id, slot) DO UPDATE SET handset_id = EXCLUDED.handset_id, state = EXCLUDED.state, written_at = now()`,
			cell, int16(r.Slot), id, state)
		return err
	})
	if err != nil {
		return Saved{}, err
	}
	if out.Enrolled {
		s.ReleaseTPUI(cell, r.TPUI) // it is the handset's now, no longer on offer
	}
	return out, nil
}

// Forget records that the handset is deregistered on cell. Its slot there is free,
// and the cell leaves its home set, or the next load would place it again. The
// handset's row stays: deleting it is an admin's decision. A pooled handset is
// deregistered on every cell, because it is the handset that forgot the system;
// the result says so, so the caller can tell the other cells.
func (s *Store) Forget(ctx context.Context, cell, id uuid.UUID) (bool, error) {
	var pooled bool
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		pooled = false
		if err := tx.QueryRow(ctx, "SELECT pooled FROM handset WHERE id = $1", id).Scan(&pooled); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if pooled {
			if _, err := tx.Exec(ctx, `UPDATE cell_slot SET handset_id = NULL, state = 'empty', written_at = now()
				WHERE handset_id = $1`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE handset SET pooled = false WHERE id = $1", id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "DELETE FROM handset_home_cell WHERE handset_id = $1", id)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cell_slot SET handset_id = NULL, state = 'empty', written_at = now()
			WHERE cell_id = $1 AND handset_id = $2`, cell, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DELETE FROM handset_home_cell WHERE cell_id = $1 AND handset_id = $2", cell, id)
		return err
	})
	return pooled, err
}

// Rename sets the handset's display name.
func (s *Store) Rename(ctx context.Context, id uuid.UUID, name string) error {
	_, err := s.Pool.Exec(ctx, "UPDATE handset SET display_name = $2 WHERE id = $1", id, name)
	return err
}

// SetCapabilities stores what the handset said of itself, as JSON, for its page.
func (s *Store) SetCapabilities(ctx context.Context, id uuid.UUID, caps map[string]any) error {
	b, err := json.Marshal(caps)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, "UPDATE handset SET capabilities = $2 WHERE id = $1", id, b)
	return err
}

// SetSignal stores the last field strength measured on a link to the handset,
// in dBm, and the time of the measurement.
func (s *Store) SetSignal(ctx context.Context, id uuid.UUID, dbm int, at time.Time) error {
	_, err := s.Pool.Exec(ctx, "UPDATE handset SET signal_dbm = $2, signal_at = $3 WHERE id = $1", id, int16(dbm), at)
	return err
}

// HandsetInfo returns the handset's display name and the id of its device
// profile ("" for the standard behaviour). The boolean is false, with a nil
// error, for a handset that does not exist.
func (s *Store) HandsetInfo(ctx context.Context, id uuid.UUID) (string, string, bool, error) {
	var name, profile string
	err := s.Pool.QueryRow(ctx, "SELECT display_name, coalesce(profile, '') FROM handset WHERE id = $1", id).Scan(&name, &profile)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return name, profile, err == nil, err
}

// Installed records that the cell's chip was written: the listed slots that hold
// a handset are "present", every other slot of the cell is "empty".
func (s *Store) Installed(ctx context.Context, cell uuid.UUID, slots []uint) error {
	in := make([]int16, len(slots))
	for i, v := range slots {
		in[i] = int16(v)
	}
	_, err := s.Pool.Exec(ctx, `UPDATE cell_slot SET written_at = now(),
			state = CASE WHEN handset_id IS NOT NULL AND slot = ANY($2) THEN 'present' ELSE 'empty' END
		WHERE cell_id = $1`, cell, in)
	return err
}

// SlotUnknown marks a slot whose write was not delivered (MAIL_REJECT): what the
// chip holds there is not known.
func (s *Store) SlotUnknown(ctx context.Context, cell uuid.UUID, slot uint) error {
	_, err := s.Pool.Exec(ctx, "UPDATE cell_slot SET state = 'unknown' WHERE cell_id = $1 AND slot = $2", cell, int16(slot))
	return err
}

// Seen notes where the handset is: its last contact, and on which cell. The
// time only moves forward, so two cells reporting out of order leave the later.
// A contact on another cell than last time is a move, and is written down.
func (s *Store) Seen(ctx context.Context, cell, id uuid.UUID, at time.Time) (uuid.UUID, error) {
	var prev *uuid.UUID
	var applied bool
	err := s.Pool.QueryRow(ctx, `WITH old AS (SELECT last_seen_cell_id, last_seen_at FROM handset WHERE id = $2 FOR UPDATE),
		up AS (UPDATE handset SET last_seen_cell_id = $1, last_seen_at = $3
			WHERE id = $2 AND (last_seen_at IS NULL OR last_seen_at <= $3) RETURNING id)
		SELECT old.last_seen_cell_id, EXISTS (SELECT 1 FROM up) FROM old`, cell, id, at).Scan(&prev, &applied)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil || prev == nil {
		return uuid.Nil, err
	}
	if applied && *prev != cell {
		if _, err := s.Pool.Exec(ctx, "INSERT INTO handset_move (handset_id, from_cell_id, to_cell_id, at) VALUES ($1, $2, $3, $4)",
			id, *prev, cell, at); err != nil {
			return *prev, err
		}
	}
	return *prev, nil
}

// LastSeenCell returns the cell the handset was last heard on, uuid.Nil if none
// or if the handset does not exist.
func (s *Store) LastSeenCell(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var c *uuid.UUID
	err := s.Pool.QueryRow(ctx, "SELECT last_seen_cell_id FROM handset WHERE id = $1", id).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) || c == nil {
		return uuid.Nil, nil
	}
	return *c, err
}

// Directory lists the handsets of these owners that are not in exclude, ordered by
// name and then id, at most limit.
func (s *Store) Directory(ctx context.Context, owners, exclude []uuid.UUID, limit int) ([]DirHandset, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT id, owner_user_id, display_name FROM handset
		  WHERE owner_user_id = ANY($1) AND NOT (id = ANY($2))
		  ORDER BY display_name, id LIMIT $3`, owners, exclude, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DirHandset
	for rows.Next() {
		var h DirHandset
		if err := rows.Scan(&h.ID, &h.Owner, &h.Name); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// InternalByNumber finds the handset of owner with internal number n. The
// boolean is false, with a nil error, when there is none; numbers are unique
// per owner, not across owners.
func (s *Store) InternalByNumber(ctx context.Context, owner uuid.UUID, n int) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := s.Pool.QueryRow(ctx, "SELECT id FROM handset WHERE owner_user_id = $1 AND internal_number = $2", owner, n).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}

// InternalIdentity returns the handset's internal number (0 when it has none),
// display name and owner. The boolean is false, with a nil error, for a handset
// that does not exist.
func (s *Store) InternalIdentity(ctx context.Context, id uuid.UUID) (int, string, uuid.UUID, bool, error) {
	var n *int16
	var name string
	var owner uuid.UUID
	err := s.Pool.QueryRow(ctx, "SELECT internal_number, display_name, owner_user_id FROM handset WHERE id = $1", id).Scan(&n, &name, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", uuid.Nil, false, nil
	}
	if err != nil {
		return 0, "", uuid.Nil, false, err
	}
	num := 0
	if n != nil {
		num = int(*n)
	}
	return num, name, owner, true, nil
}

// Detached notes that the handset said it is off. last_seen_at moves with it: the message is a contact.
func (s *Store) Detached(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := s.Pool.Exec(ctx, `UPDATE handset SET detached_at = $2, last_seen_at = greatest(last_seen_at, $2)
		WHERE id = $1 AND (detached_at IS NULL OR detached_at < $2)`, id, at)
	return err
}

// PageFailed notes that a page for the handset went unanswered.
func (s *Store) PageFailed(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := s.Pool.Exec(ctx, `UPDATE handset SET page_failed_at = $2 WHERE id = $1 AND (page_failed_at IS NULL OR page_failed_at < $2)`, id, at)
	return err
}

// Dropped is a record that left a cell's table because the owner does not trust the cell's
// operator any more.
type Dropped struct {
	Cell, Handset uuid.UUID
	Slot          int16 // a chip slot (up to maxSlot) keeps the key in the chip until it is overwritten
}

// DropUntrusted removes every record from a cell whose operator its handset's owner does not
// trust (handset_may_use_cell), and takes the cell out of the home set of such handsets so that a
// restart does not place them again. It returns what it removed, for the cells' stacks to follow.
func (s *Store) DropUntrusted(ctx context.Context) ([]Dropped, error) {
	var out []Dropped
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		out = nil
		if _, err := tx.Exec(ctx, `DELETE FROM handset_home_cell hc USING handset h
			WHERE h.id = hc.handset_id AND NOT handset_may_use_cell(h.owner_user_id, hc.cell_id)`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE cell_slot cs SET handset_id = NULL, state = 'empty', written_at = now()
			FROM handset h
			WHERE h.id = cs.handset_id AND NOT handset_may_use_cell(h.owner_user_id, cs.cell_id)
			RETURNING cs.cell_id, h.id, cs.slot`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Dropped, error) {
			var d Dropped
			err := r.Scan(&d.Cell, &d.Handset, &d.Slot)
			return d, err
		})
		return err
	})
	return out, err
}
