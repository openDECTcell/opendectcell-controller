// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"errors"
	"testing"
)

func TestImportHandsetAndExportKey(t *testing.T) {
	s, ctx := newStore(t)
	anna, ben := s.newUser(t, ctx, "anna"), s.newUser(t, ctx, "ben")
	in := Imported{IPUI: []byte{0x0a}, Name: "Hall", DualRing: "busy", Key: Key{CCPortID: []byte{1, 2}, CCFixedID: []byte{3}}}
	for i := range in.UAK {
		in.UAK[i] = byte(i + 1)
	}
	id, err := s.ImportHandset(ctx, anna, in)
	if err != nil {
		t.Fatal(err)
	}
	k, err := s.ExportKey(ctx, anna, id)
	if err != nil || k.UAK != in.UAK || string(k.CCPortID) != "\x01\x02" || string(k.CCFixedID) != "\x03" {
		t.Fatalf("key: %+v %v", k, err)
	}
	// the key is the owner's: nobody else reads it
	if _, err := s.ExportKey(ctx, ben, id); err == nil {
		t.Error("another user read the key")
	}
	// a known identity is refused, whoever asks, and the key stays
	in.UAK[0] = 0xee
	if _, err := s.ImportHandset(ctx, ben, in); !errors.Is(err, ErrKnownIPUI) {
		t.Errorf("known identity: %v", err)
	}
	if _, err := s.ImportHandset(ctx, anna, in); !errors.Is(err, ErrKnownIPUI) {
		t.Errorf("known identity, same owner: %v", err)
	}
	if k, _ := s.ExportKey(ctx, anna, id); k.UAK[0] != 1 {
		t.Error("the key was replaced")
	}
	// it is held by no cell yet: the first cell that hears it places it
	cell := s.newCell(t, ctx, "c1")
	h, ok, err := s.PlaceByIPUI(ctx, cell, in.IPUI)
	if err != nil || !ok || h.Rec.UAK != k.UAK {
		t.Fatalf("place: %v %v %+v", err, ok, h.Rec.UAK)
	}
}
