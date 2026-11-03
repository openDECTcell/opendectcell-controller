// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"testing"
)

// Only HashConcurrency password hashes run at once; a waiter gives up when its
// request ends.
func TestHashSlotsAreBounded(t *testing.T) {
	s := &Store{HashConcurrency: 2}
	r1, err := s.hashSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.hashSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.hashSlot(gone); !errors.Is(err, context.Canceled) {
		t.Fatalf("a third hash did not wait: %v", err)
	}
	r1()
	r3, err := s.hashSlot(context.Background())
	if err != nil {
		t.Fatalf("a freed place was not usable: %v", err)
	}
	r2()
	r3()
}
