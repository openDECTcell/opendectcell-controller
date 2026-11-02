// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	dectcore "github.com/openDECTcell/opendectcell-controller/internal/dect/core"
)

// phonebook is the contacts store as the DECT core reads it (core.Contacts).
type phonebook struct{ s *contacts.Store }

func (p phonebook) ListFile(ctx context.Context, h uuid.UUID, seq uint32, hints map[int64]uint16) (string, error) {
	return p.s.ListFile(ctx, h, seq, hints)
}

func (p phonebook) ApplyChange(ctx context.Context, h uuid.UUID, line string) (dectcore.ContactsApplied, error) {
	a, err := p.s.ApplyChange(ctx, h, line)
	return dectcore.ContactsApplied{Seq: a.Seq, Contact: a.Contact, Entry: a.Entry}, err
}
