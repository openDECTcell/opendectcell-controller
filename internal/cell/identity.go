// SPDX-License-Identifier: AGPL-3.0-or-later

package cell

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

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

// SystemIdentity is a class B ARI and where it came from.
type SystemIdentity struct {
	EIC    uint16    `json:"eic"`
	FPN    uint8     `json:"fpn"`
	FPS    uint8     `json:"fps"`
	Source string    `json:"source"`
	SetBy  string    `json:"set_by,omitempty"`
	SetAt  time.Time `json:"set_at"`
}

// ErrBadIdentity: a value EN 300 175-6 §5.2 does not allow.
var ErrBadIdentity = errors.New("cell: invalid system identity")

// Validate checks the ranges of §5.2: EIC 0 "shall not be used", FPS 0 is
// "reserved for future use" (so FPN + FPS is never 0 either).
func (id SystemIdentity) Validate() error {
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
	v := uint64(id.ARI())<<8 | uint64(rpn)
	var out [5]byte
	for i := 4; i >= 0; i-- {
		out[i] = byte(v)
		v >>= 8
	}
	return out
}

func (id SystemIdentity) String() string {
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
