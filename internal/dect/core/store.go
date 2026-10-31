// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"crypto/rand"
	"encoding/binary"
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
	Rec   stack.Record // Rec.Slot is the cell's chip slot
}

// Records is where the core keeps handset records (Store in production).
type Records interface {
	// CellRecords is what cell holds, at most max records, placed by
	// cell-protocol.md §7.4 rules 1 and 2 (home set, no eviction). unplaced
	// lists home-set handsets that found no free slot.
	CellRecords(ctx context.Context, cell uuid.UUID, max int) (hs []Handset, unplaced []uuid.UUID, err error)
	// SaveRecord stores a record the stack changed. id uuid.Nil is a
	// handset that just enrolled on cell through owner's registration
	// window; its id is returned. owner is ignored for a known id.
	SaveRecord(ctx context.Context, cell, id, owner uuid.UUID, r stack.Record) (Saved, error)
	// PlaceHandset puts a pooled handset on cell too, at the lowest free
	// record number above the chip's slots. ok is false for a handset that is
	// not pooled, already placed on cell, or finds no number free.
	PlaceHandset(ctx context.Context, cell, id uuid.UUID) (h Handset, ok bool, err error)
	// ReserveTPUI takes a TPUI from the pool for an enrolment a cell may
	// take; all zero when the pool is off. ReleaseTPUI gives it back unused.
	ReserveTPUI(ctx context.Context) ([3]byte, error)
	ReleaseTPUI(t [3]byte)
	// Forget: a handset deregistered on cell; it is no longer placed there.
	// A pooled handset is deregistered everywhere (pooled says so, and the
	// other cells must drop it).
	Forget(ctx context.Context, cell, id uuid.UUID) (pooled bool, err error)
	// Rename sets a handset's name.
	Rename(ctx context.Context, id uuid.UUID, name string) error
	// Installed: the cell's chip was written: the slots listed hold their
	// handsets, every other slot of the cell is empty.
	Installed(ctx context.Context, cell uuid.UUID, slots []uint) error
	// SlotUnknown: a write to slot was not delivered.
	SlotUnknown(ctx context.Context, cell uuid.UUID, slot uint) error
	// LastSeenCell is the cell the handset was last heard on, uuid.Nil if
	// none.
	LastSeenCell(ctx context.Context, id uuid.UUID) (uuid.UUID, error)
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
	reserved map[uint8]bool // pool TPUIs offered to a free record, by low byte
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
	poolFirst = maxSlot + 1 // the first TPUI low byte of the pool: 11 01 to 11 06 are the chip's
)

// poolTPUI is a pool TPUI {00, 11, N}, N from 7, as the DB column holds it.
func poolTPUI(t [3]byte) (int32, bool) {
	if t[0] != 0 || t[1] != 0x11 || t[2] < poolFirst {
		return 0, false
	}
	return 0x1100 | int32(t[2]), true
}

func tpuiBytes(v int32) [3]byte { return [3]byte{byte(v >> 16), byte(v >> 8), byte(v)} }

// ErrPoolFull: every TPUI of the pool is held (249 handsets).
var ErrPoolFull = errors.New("core: the TPUI pool is full (249 pooled handsets)")

func (s *Store) ReserveTPUI(ctx context.Context) ([3]byte, error) {
	if s.NoPool {
		return [3]byte{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.Pool.Query(ctx, "SELECT (tpui & 255)::int FROM handset WHERE tpui & 65280 = 4352")
	if err != nil {
		return [3]byte{}, err
	}
	taken, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return [3]byte{}, err
	}
	used := map[int]bool{}
	for _, t := range taken {
		used[t] = true
	}
	var free []uint8
	for n := poolFirst; n <= 255; n++ {
		if !used[n] && !s.reserved[uint8(n)] {
			free = append(free, uint8(n))
		}
	}
	if len(free) == 0 {
		return [3]byte{}, ErrPoolFull
	}
	k, err := rand.Int(rand.Reader, big.NewInt(int64(len(free))))
	if err != nil {
		return [3]byte{}, err
	}
	if s.reserved == nil {
		s.reserved = map[uint8]bool{}
	}
	n := free[k.Int64()]
	s.reserved[n] = true
	return [3]byte{0, 0x11, n}, nil
}

func (s *Store) ReleaseTPUI(t [3]byte) {
	if t[1] != 0x11 {
		return
	}
	s.mu.Lock()
	delete(s.reserved, t[2])
	s.mu.Unlock()
}

func (s *Store) CellRecords(ctx context.Context, cell uuid.UUID, max int) ([]Handset, []uuid.UUID, error) {
	max = min(max, maxSlot)
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
			WHERE hc.cell_id = $1 AND NOT h.pooled AND NOT EXISTS
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
			for used[slot] && slot <= int16(max) {
				slot++
			}
			if slot > int16(max) {
				unplaced = append(unplaced, id)
				continue
			}
			used[slot] = true
			if err := putSlot(ctx, tx, cell, slot, id, false); err != nil {
				return err
			}
		}
		// A pooled handset is held by every cell, host-only: one that sits
		// in a chip slot here (it enrolled in this run) gives the slot up,
		// and every one without a place gets the lowest number above the
		// chip's slots.
		if _, err := tx.Exec(ctx, `UPDATE cell_slot cs SET handset_id = NULL, state = 'empty', written_at = now()
			FROM handset h WHERE cs.cell_id = $1 AND cs.handset_id = h.id AND h.pooled AND cs.slot <= $2`,
			cell, int16(maxSlot)); err != nil {
			return err
		}
		if used, err = usedSlots(ctx, tx, cell); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT h.id FROM handset h WHERE h.pooled AND NOT EXISTS
				(SELECT 1 FROM cell_slot cs WHERE cs.cell_id = $1 AND cs.handset_id = h.id)
			ORDER BY h.registered_at, h.id`, cell)
		if err != nil {
			return err
		}
		pooled, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		for _, id := range pooled {
			slot := freeHostSlot(used)
			if slot == 0 {
				unplaced = append(unplaced, id)
				continue
			}
			used[slot] = true
			if err := putSlot(ctx, tx, cell, slot, id, true); err != nil {
				return err
			}
		}
		hs, err = s.readRecords(ctx, tx, `WHERE cs.cell_id = $1 AND cs.slot BETWEEN 1 AND $2 ORDER BY cs.slot`, cell, int16(maxRecord))
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

// PlaceHandset is CellRecords for one pooled handset that a cell already
// running has not got.
func (s *Store) PlaceHandset(ctx context.Context, cell, id uuid.UUID) (Handset, bool, error) {
	var out []Handset
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		out = nil
		if err := lockCell(ctx, tx, cell); err != nil {
			return err
		}
		var pooled, here bool
		if err := tx.QueryRow(ctx, `SELECT h.pooled, EXISTS (SELECT 1 FROM cell_slot cs WHERE cs.cell_id = $1 AND cs.handset_id = h.id)
			FROM handset h WHERE h.id = $2`, cell, id).Scan(&pooled, &here); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if !pooled || here {
			return nil
		}
		used, err := usedSlots(ctx, tx, cell)
		if err != nil {
			return err
		}
		slot := freeHostSlot(used)
		if slot == 0 {
			return nil
		}
		if err := putSlot(ctx, tx, cell, slot, id, true); err != nil {
			return err
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
			coalesce(h.cc_port_id, ''::bytea), coalesce(h.cc_fixed_id, ''::bytea), h.pooled, h.tpui
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
			&h.Rec.CCPortID, &h.Rec.CCFixedID, &pooled, &tpui); err != nil {
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
			// offer (ReserveTPUI): the handset is held by every cell.
			pt, pooled := poolTPUI(r.TPUI)
			pooled = pooled && !s.NoPool
			// A handset enrolling again keeps its row: same IPUI, new key.
			// Only its owner can do that; a change of owner is not built.
			var was uuid.UUID
			err := tx.QueryRow(ctx, "SELECT id, owner_user_id FROM handset WHERE ipui = $1", r.PortID).Scan(&id, &was)
			if err == nil && was != owner {
				return errOtherOwner
			}
			if errors.Is(err, pgx.ErrNoRows) {
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
			} else if err != nil {
				return err
			} else {
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
		s.ReleaseTPUI(r.TPUI) // it is the handset's now, no longer on offer
	}
	return out, nil
}

// Forget: the handset is deregistered on cell. Its slot there is free, and
// the cell leaves its home set, or the next load would place it again. The
// handset's row stays; deleting it is an admin's decision. A pooled handset
// is deregistered on every cell: it is the handset that forgot the system.
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

func (s *Store) Rename(ctx context.Context, id uuid.UUID, name string) error {
	_, err := s.Pool.Exec(ctx, "UPDATE handset SET display_name = $2 WHERE id = $1", id, name)
	return err
}

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

func (s *Store) LastSeenCell(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var c *uuid.UUID
	err := s.Pool.QueryRow(ctx, "SELECT last_seen_cell_id FROM handset WHERE id = $1", id).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) || c == nil {
		return uuid.Nil, nil
	}
	return *c, err
}
