// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Key is the long-term key and the identities of a handset, for an export the owner asked for (internal/transfer).
type Key struct {
	UAK       [16]byte
	CCPortID  []byte
	CCFixedID []byte
}

// ErrKnownIPUI means a handset with this identity is already registered here.
var ErrKnownIPUI = errors.New("core: a handset with this identity is registered here already")

// ExportKey opens the key of one of owner's handsets. The caller clears it after use; it is never logged.
func (s *Store) ExportKey(ctx context.Context, owner, id uuid.UUID) (Key, error) {
	var enc []byte
	var k Key
	err := s.Pool.QueryRow(ctx, `SELECT uak_enc, coalesce(cc_port_id, ''::bytea), coalesce(cc_fixed_id, ''::bytea)
		FROM handset WHERE id = $1 AND owner_user_id = $2`, id, owner).Scan(&enc, &k.CCPortID, &k.CCFixedID)
	if err != nil {
		return Key{}, err
	}
	plain, err := s.Keyring.Open(uakLoc(id), enc)
	if err != nil {
		return Key{}, err
	}
	defer clear(plain)
	if len(plain) != len(k.UAK) {
		return Key{}, errors.New("core: stored key has the wrong length")
	}
	copy(k.UAK[:], plain)
	return k, nil
}

// Imported is a handset to be registered from an export file: the identity and key it had, no air interface involved.
type Imported struct {
	IPUI []byte
	Key
	Name, Model, Profile string
	EMC                  *int
	DefaultLine          *int
	DualRing             string
	InternalNumber       *int
}

// ImportHandset registers a handset for owner from its exported key. It gets a TPUI of this controller and no cell slot: the first
// cell that hears it takes it in (PlaceByIPUI). A handset whose identity is known here is refused (ErrKnownIPUI), whoever owns it.
func (s *Store) ImportHandset(ctx context.Context, owner uuid.UUID, in Imported) (uuid.UUID, error) {
	id := uuid.New()
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var known bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM handset WHERE ipui = $1)", in.IPUI).Scan(&known); err != nil {
			return err
		}
		if known {
			return ErrKnownIPUI
		}
		tpui, err := newTPUI(ctx, tx)
		if err != nil {
			return err
		}
		enc, err := s.Keyring.Seal(uakLoc(id), in.UAK[:])
		if err != nil {
			return err
		}
		dual := in.DualRing
		if dual == "" {
			dual = "call_waiting"
		}
		_, err = tx.Exec(ctx, `INSERT INTO handset (id, owner_user_id, ipui, tpui, uak_enc, display_name, model, profile, manufacturer_emc,
				default_line, dual_ring_policy, internal_number, cc_port_id, cc_fixed_id)
			VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), $9, $10, $11, $12, $13, $14)`,
			id, owner, in.IPUI, tpui, enc, in.Name, in.Model, in.Profile, in.EMC, in.DefaultLine, dual, in.InternalNumber,
			nullIfEmpty(in.CCPortID), nullIfEmpty(in.CCFixedID))
		return err
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}
