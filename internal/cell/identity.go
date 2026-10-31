// SPDX-License-Identifier: AGPL-3.0-or-later

package cell

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

// The DECT system identity this controller's cells share: an ARI of
// class B (EN 300 175-6 §5.2), so one system can have up to 256 cells and
// a handset subscribed once has access rights on every one. openDECTcell
// docs/multicell-identities.md §7.7 has the reasoning.
//
// EICs are allocated by ETSI. An admin may also make one up, for an
// experiment: the maintainer's decision of 2026-10-30, with the risks
// shown on the admin page (a made-up EIC may be a real system's; nearby,
// handsets of either may try the other's base). ETSI publishes no list of
// allocated EICs, so there is nothing to check one against.
//
// Not applied to cells yet: a cell's chip takes its RFPI when its firmware
// is uploaded, and the cell side does not take one from the controller so
// far (multicell-identities.md §7.4).

const (
	settingIdentityKey = "dect_system_identity"

	// IdentityRandom: made up on this controller. IdentityAllocated: an
	// EIC bought from ETSI.
	IdentityRandom    = "random"
	IdentityAllocated = "allocated"

	// IdentityPLI is the PARK length handsets are given: ARC and EIC, so
	// the access rights cover every system under the EIC (§5.2, §6.1.2).
	IdentityPLI = 19
)

// SystemIdentity is an ARI of class B (EIC, FPN, FPS), or of class A: the
// factory ARI of one cell, the donor (openDECTcell docs/multicell-identities.md §7.1,
// at most 7 cells). Apply says the cells are to take it: until an admin turns
// that on, every cell keeps its box's own identity and nothing changes.
type SystemIdentity struct {
	// Class is "B" (also when empty, as stored before class A existed) or "A".
	Class  string    `json:"class,omitempty"`
	EIC    uint16    `json:"eic"`
	FPN    uint8     `json:"fpn"`
	FPS    uint8     `json:"fps"`
	Donor  string    `json:"donor,omitempty"` // class A: the donor's RFPI, hex, RPN 0
	Source string    `json:"source"`
	Apply  bool      `json:"apply,omitempty"`
	SetBy  string    `json:"set_by,omitempty"`
	SetAt  time.Time `json:"set_at"`
}

// IdentityDonor is the source of a class A identity taken from a cell.
const IdentityDonor = "donor"

// IsA reports class A.
func (id SystemIdentity) IsA() bool { return id.Class == "A" }

// MaxRPN is the highest cell number: 7 for class A (3 bits, 0 is "single
// cell"), 255 for class B.
func (id SystemIdentity) MaxRPN() int {
	if id.IsA() {
		return 7
	}
	return 255
}

// ParkPLI is the PARK length handsets get: ARC and EIC for class B, the
// class's whole ARI (0) for class A.
func (id SystemIdentity) ParkPLI() uint {
	if id.IsA() {
		return 0
	}
	return IdentityPLI
}

func (id SystemIdentity) donorBytes() ([5]byte, error) {
	var out [5]byte
	b, err := hex.DecodeString(id.Donor)
	if err != nil || len(b) != 5 {
		return out, fmt.Errorf("%w: the donor RFPI is 10 hex digits", ErrBadIdentity)
	}
	copy(out[:], b)
	return out, nil
}

// ErrBadIdentity: a value EN 300 175-6 §5.2 does not allow.
var ErrBadIdentity = errors.New("cell: invalid system identity")

// Validate checks the ranges of §5.2: EIC 0 "shall not be used", FPS 0 is
// "reserved for future use" (so FPN + FPS is never 0 either).
func (id SystemIdentity) Validate() error {
	if id.IsA() {
		d, err := id.donorBytes()
		switch {
		case err != nil:
			return err
		case d[0]&0xF0 != 0:
			return fmt.Errorf("%w: the donor's RFPI is not class A with E 0", ErrBadIdentity)
		case d[4]&0x07 != 0:
			return fmt.Errorf("%w: the donor's RPN is not 0", ErrBadIdentity)
		case id.Source != IdentityDonor:
			return fmt.Errorf("%w: unknown source %q", ErrBadIdentity, id.Source)
		}
		return nil
	}
	switch {
	case id.EIC == 0:
		return fmt.Errorf("%w: EIC 0 is not used", ErrBadIdentity)
	case id.FPS < 1 || id.FPS > 15:
		return fmt.Errorf("%w: FPS must be 1 to 15", ErrBadIdentity)
	case id.Source != IdentityRandom && id.Source != IdentityAllocated:
		return fmt.Errorf("%w: unknown source %q", ErrBadIdentity, id.Source)
	}
	return nil
}

// RandomIdentity makes one up: EIC 1..65535, FPN 0..255, FPS 1..15.
func RandomIdentity(rnd io.Reader) (SystemIdentity, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	var b [4]byte
	for {
		if _, err := io.ReadFull(rnd, b[:]); err != nil {
			return SystemIdentity{}, err
		}
		eic := binary.BigEndian.Uint16(b[:2])
		if eic == 0 {
			continue
		}
		return SystemIdentity{EIC: eic, FPN: b[2], FPS: 1 + b[3]%15, Source: IdentityRandom}, nil
	}
}

// ARI is the 31-bit ARI: ARC 001 (class B), EIC, FPN, FPS (figure 5.8).
func (id SystemIdentity) ARI() uint32 {
	return 1<<28 | uint32(id.EIC)<<12 | uint32(id.FPN)<<4 | uint32(id.FPS)
}

// RFPI is the 40-bit RFPI of the cell with this RPN, most significant
// byte first: E = 0 (no SARIs), the ARI, the RPN (figure 5.8).
func (id SystemIdentity) RFPI(rpn uint8) [5]byte {
	if id.IsA() {
		d, _ := id.donorBytes()
		d[4] = d[4]&^0x07 | rpn&0x07
		return d
	}
	v := uint64(id.ARI())<<8 | uint64(rpn)
	var out [5]byte
	for i := 4; i >= 0; i-- {
		out[i] = byte(v)
		v >>= 8
	}
	return out
}

func (id SystemIdentity) String() string {
	if id.IsA() {
		return "class A, taken from a cell (ARI " + id.Donor + ")"
	}
	return fmt.Sprintf("class B, EIC %04X, FPN %d, FPS %d", id.EIC, id.FPN, id.FPS)
}

// SystemIdentity returns the installation's identity, or nil if none is set.
func (s *Store) SystemIdentity(ctx context.Context) (*SystemIdentity, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx, "SELECT value FROM setting WHERE key = $1", settingIdentityKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var id SystemIdentity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// SetSystemIdentity stores the identity (admins only), audited with the
// one it replaces.
func (s *Store) SetSystemIdentity(ctx context.Context, u *auth.User, id SystemIdentity) error {
	if auth.RequireAdmin(u) != nil {
		return ErrNotFound
	}
	if err := id.Validate(); err != nil {
		return err
	}
	id.SetBy, id.SetAt = u.Username, s.now()
	v, err := json.Marshal(id)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var prev []byte
		err := tx.QueryRow(ctx, "SELECT value FROM setting WHERE key = $1 FOR UPDATE", settingIdentityKey).Scan(&prev)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO setting (key, value, updated_at) VALUES ($1, $2, $3)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
			settingIdentityKey, v, id.SetAt); err != nil {
			return err
		}
		details := map[string]any{"identity": id.String(), "source": id.Source}
		if prev != nil {
			details["previous"] = string(prev)
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "dect_identity.set", ObjectType: "setting",
			ObjectID: settingIdentityKey, Details: details})
	})
}

// SetIdentityApply turns the cells' use of the system identity on or off
// (admins only). Turning it on needs an identity. Each cell is told the next
// time it connects, and on turning it off gets its own identity back.
func (s *Store) SetIdentityApply(ctx context.Context, u *auth.User, on bool) error {
	if auth.RequireAdmin(u) != nil {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx, "SELECT value FROM setting WHERE key = $1 FOR UPDATE", settingIdentityKey).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			if on {
				return fmt.Errorf("%w: there is none to apply", ErrBadIdentity)
			}
			return nil
		}
		if err != nil {
			return err
		}
		var id SystemIdentity
		if err := json.Unmarshal(raw, &id); err != nil {
			return err
		}
		if id.Apply == on {
			return nil
		}
		id.Apply = on
		v, err := json.Marshal(id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE setting SET value = $2, updated_at = $3 WHERE key = $1", settingIdentityKey, v, s.now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "dect_identity.apply", ObjectType: "setting",
			ObjectID: settingIdentityKey, Details: map[string]any{"apply": on, "identity": id.String()}})
	})
}

// ErrNoRPN means every cell number of the identity's class is taken.
var ErrNoRPN = errors.New("cell: no free RPN for another cell in this system")

// AssignRPN returns the cell's RPN, giving it the lowest free one from 1 up
// the first time (or when its number does not fit the identity's class any
// more). The number stays with the cell until it is revoked.
func (s *Store) AssignRPN(ctx context.Context, id SystemIdentity, cellID uuid.UUID) (uint8, error) {
	var rpn uint8
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('cell_rpn'))"); err != nil {
			return err
		}
		var cur *int16
		if err := tx.QueryRow(ctx, "SELECT rpn FROM cell WHERE id = $1", cellID).Scan(&cur); err != nil {
			return err
		}
		if cur != nil && *cur >= 1 && int(*cur) <= id.MaxRPN() {
			rpn = uint8(*cur)
			return nil
		}
		rows, err := tx.Query(ctx, "SELECT rpn FROM cell WHERE rpn IS NOT NULL AND id <> $1", cellID)
		if err != nil {
			return err
		}
		used := map[int16]bool{}
		for rows.Next() {
			var v int16
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			used[v] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for n := 1; n <= id.MaxRPN(); n++ {
			if !used[int16(n)] {
				rpn = uint8(n)
				_, err := tx.Exec(ctx, "UPDATE cell SET rpn = $2 WHERE id = $1", cellID, int16(n))
				return err
			}
		}
		return ErrNoRPN
	})
	return rpn, err
}

// IdentityWant is what a cell should broadcast.
type IdentityWant struct {
	RFPI    [5]byte
	ParkPLI uint
	// Set: the cell is to take RFPI. Revert: it has an identity from us and
	// is to go back to its own. Neither: leave it.
	Set, Revert bool
}

// Want says what cellID should broadcast: the system identity with its RPN
// when an admin turned it on, the way back for a cell that took it before,
// nothing otherwise.
func (s *Store) Want(ctx context.Context, cellID uuid.UUID) (IdentityWant, error) {
	var w IdentityWant
	id, err := s.SystemIdentity(ctx)
	if err != nil {
		return w, err
	}
	if id == nil || !id.Apply {
		var pushed []byte
		if err := s.Pool.QueryRow(ctx, "SELECT rfpi_pushed FROM cell WHERE id = $1", cellID).Scan(&pushed); err != nil {
			return w, err
		}
		w.Revert = pushed != nil
		return w, nil
	}
	rpn, err := s.AssignRPN(ctx, *id, cellID)
	if err != nil {
		return w, err
	}
	w.RFPI, w.ParkPLI, w.Set = id.RFPI(rpn), id.ParkPLI(), true
	return w, nil
}

// Pushed records that the cell was told to broadcast rfpi (nil: told to go back
// to its own).
func (s *Store) Pushed(ctx context.Context, cellID uuid.UUID, rfpi *[5]byte) error {
	var v []byte
	if rfpi != nil {
		v = rfpi[:]
	}
	_, err := s.Pool.Exec(ctx, "UPDATE cell SET rfpi_pushed = $2 WHERE id = $1", cellID, v)
	return err
}

// SystemIdentityFromCell makes a class A identity of a cell's own RFPI, the
// donor's (admins only). The cell must still broadcast its factory identity:
// the caller checks that the identity is not applied.
func (s *Store) SystemIdentityFromCell(ctx context.Context, u *auth.User, rfpi [5]byte) error {
	rfpi[4] &^= 0x07 // the donor's RPN is the cells' to assign
	return s.SetSystemIdentity(ctx, u, SystemIdentity{Class: "A", Donor: hex.EncodeToString(rfpi[:]), Source: IdentityDonor})
}
