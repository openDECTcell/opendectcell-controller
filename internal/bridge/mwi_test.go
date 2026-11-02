// SPDX-License-Identifier: AGPL-3.0-or-later

package bridge

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/core"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
)

type mwiGot struct {
	hs    uuid.UUID
	line  int
	count uint
}

type fakeMWI struct {
	mu  sync.Mutex
	got []mwiGot
}

func (f *fakeMWI) SendMWI(hid uuid.UUID, line int, count uint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, mwiGot{hid, line, count})
}

// An account's message count goes to every handset that has the account as a line,
// on that line; a handset that asks later gets the count of its lines whose account
// has reported.
func TestAccountMWI(t *testing.T) {
	acct, other := uuid.New(), uuid.New()
	h1, h2, h3 := uuid.New(), uuid.New(), uuid.New()
	snd := &fakeMWI{}
	m := &Manager{MWI: snd, Lines: fakeLines{
		recv:   []Receiver{{Handset: h1, Line: 1}, {Handset: h2, Line: 2}},
		hlines: []HandsetLine{{Line: 1, Account: other}, {Line: 2, Account: acct}},
	}}
	if got := m.MWIFor(context.Background(), h3); len(got) != 0 {
		t.Errorf("before any report: %+v", got)
	}
	m.AccountMWI(acct, sip.MWI{Waiting: true, New: 3})
	want := []mwiGot{{h1, 1, 3}, {h2, 2, 3}}
	if !slices.Equal(snd.got, want) {
		t.Errorf("sent %+v, want %+v", snd.got, want)
	}
	// h3's line 2 is that account: it is told at once what the mailbox holds; line 1's account has not reported
	if got := m.MWIFor(context.Background(), h3); !slices.Equal(got, []core.MWILine{{Line: 2, Count: 3}}) {
		t.Errorf("for a handset that comes later: %+v", got)
	}
	m.AccountMWI(acct, sip.MWI{})
	if got := m.MWIFor(context.Background(), h3); !slices.Equal(got, []core.MWILine{{Line: 2, Count: 0}}) {
		t.Errorf("after the messages were heard: %+v", got)
	}
}
