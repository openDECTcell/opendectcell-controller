// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

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
	Owner *uuid.UUID // nil: a shared, admin-managed handset
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
	// handset that just enrolled on cell; its id is returned.
	SaveRecord(ctx context.Context, cell, id uuid.UUID, r stack.Record) (uuid.UUID, error)
	// Forget frees a deregistered handset's slot on cell.
	Forget(ctx context.Context, cell, id uuid.UUID) error
	// Rename sets a handset's name.
	Rename(ctx context.Context, id uuid.UUID, name string) error
	// Installed: the cell's chip was written: the slots listed hold their
	// handsets, every other slot of the cell is empty.
	Installed(ctx context.Context, cell uuid.UUID, slots []uint) error
	// SlotUnknown: a write to slot was not delivered.
	SlotUnknown(ctx context.Context, cell uuid.UUID, slot uint) error
}

// Store keeps the records in PostgreSQL, the UAK sealed (AGENTS.md §5).
type Store struct {
	Pool    *pgxpool.Pool
	Keyring *secret.Keyring
}

var _ Records = (*Store)(nil)

func uakLoc(id uuid.UUID) secret.Location {
	return secret.Location{Table: "handset", Column: "uak_enc", RowID: id.String()}
}

// maxSlot is the chip's subscription table (DECT_MM_MAX_SLOT).
const maxSlot = 6

func (s *Store) CellRecords(ctx context.Context, cell uuid.UUID, max int) ([]Handset, []uuid.UUID, error) {
	max = min(max, maxSlot)
	var hs []Handset
	var unplaced []uuid.UUID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		hs, unplaced = nil, nil
		// One placement at a time per cell.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('cell_slot'), hashtext($1))", cell.String()); err != nil {
			return err
		}
		// Home-set handsets without a slot on this cell get the lowest free one.
		rows, err := tx.Query(ctx, `SELECT hc.handset_id FROM handset_home_cell hc JOIN handset h ON h.id = hc.handset_id
			WHERE hc.cell_id = $1 AND NOT EXISTS
				(SELECT 1 FROM cell_slot cs WHERE cs.cell_id = $1 AND cs.handset_id = hc.handset_id)
			ORDER BY h.registered_at, h.id`, cell)
		if err != nil {
			return err
		}
		homeless, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, "SELECT slot FROM cell_slot WHERE cell_id = $1 AND handset_id IS NOT NULL", cell)
		if err != nil {
			return err
		}
		taken, err := pgx.CollectRows(rows, pgx.RowTo[int16])
		if err != nil {
			return err
		}
		used := map[int16]bool{}
		for _, t := range taken {
			used[t] = true
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
			if _, err := tx.Exec(ctx, `INSERT INTO cell_slot (cell_id, slot, handset_id, state) VALUES ($1, $2, $3, 'unknown')
				ON CONFLICT (cell_id, slot) DO UPDATE SET handset_id = EXCLUDED.handset_id, state = 'unknown'`,
				cell, slot, id); err != nil {
				return err
			}
		}
		rows, err = tx.Query(ctx, `SELECT h.id, h.owner_user_id, h.display_name, cs.slot, h.ipui, h.uak_enc,
				coalesce(h.cc_port_id, ''::bytea), coalesce(h.cc_fixed_id, ''::bytea)
			FROM cell_slot cs JOIN handset h ON h.id = cs.handset_id
			WHERE cs.cell_id = $1 AND cs.slot BETWEEN 1 AND $2 ORDER BY cs.slot`, cell, max)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h Handset
			var slot int16
			var enc []byte
			if err := rows.Scan(&h.ID, &h.Owner, &h.Name, &slot, &h.Rec.PortID, &enc,
				&h.Rec.CCPortID, &h.Rec.CCFixedID); err != nil {
				return err
			}
			uak, err := s.Keyring.Open(uakLoc(h.ID), enc)
			if err != nil {
				return fmt.Errorf("handset %s: UAK: %w", h.ID, err)
			}
			if len(uak) != len(h.Rec.UAK) {
				clear(uak)
				return fmt.Errorf("handset %s: UAK of %d bytes", h.ID, len(uak))
			}
			copy(h.Rec.UAK[:], uak)
			clear(uak)
			h.Rec.Slot = uint(slot)
			hs = append(hs, h)
		}
		return rows.Err()
	})
	if err != nil {
		for i := range hs {
			clear(hs[i].Rec.UAK[:])
		}
		return nil, nil, err
	}
	return hs, unplaced, nil
}

// errTPUI means no free TPUI was found; it does not happen in practice.
var errTPUI = errors.New("core: no free TPUI found")

// newTPUI picks a TPUI from the installation-wide pool whose low 16 bits,
// the link signature, are unused too (cell-protocol.md §7.4).
func newTPUI(ctx context.Context, tx pgx.Tx) (int32, error) {
	var b [4]byte
	for range 32 {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		t := int32(binary.BigEndian.Uint32(b[:]) & 0xFFFFF)
		var used bool
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

func (s *Store) SaveRecord(ctx context.Context, cell, id uuid.UUID, r stack.Record) (uuid.UUID, error) {
	if r.Slot < 1 || r.Slot > maxSlot {
		return uuid.Nil, fmt.Errorf("core: record in chip slot %d", r.Slot)
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if id == uuid.Nil {
			if len(r.PortID) == 0 {
				return errors.New("core: an enrolled record without an identity")
			}
			// A handset enrolling again keeps its row: same IPUI, new key.
			err := tx.QueryRow(ctx, "SELECT id FROM handset WHERE ipui = $1", r.PortID).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				id = uuid.New()
				tpui, err := newTPUI(ctx, tx)
				if err != nil {
					return err
				}
				enc, err := s.Keyring.Seal(uakLoc(id), r.UAK[:])
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO handset (id, ipui, tpui, uak_enc, cc_port_id, cc_fixed_id)
					VALUES ($1, $2, $3, $4, $5, $6)`, id, r.PortID, tpui, enc,
					nullIfEmpty(r.CCPortID), nullIfEmpty(r.CCFixedID)); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO handset_home_cell (handset_id, cell_id) VALUES ($1, $2)
					ON CONFLICT DO NOTHING`, id, cell); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
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
		_, err = tx.Exec(ctx, `INSERT INTO cell_slot (cell_id, slot, handset_id, state, written_at) VALUES ($1, $2, $3, 'present', now())
			ON CONFLICT (cell_id, slot) DO UPDATE SET handset_id = EXCLUDED.handset_id, state = 'present', written_at = now()`,
			cell, int16(r.Slot), id)
		return err
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (s *Store) Forget(ctx context.Context, cell, id uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `UPDATE cell_slot SET handset_id = NULL, state = 'empty', written_at = now()
		WHERE cell_id = $1 AND handset_id = $2`, cell, id)
	return err
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
