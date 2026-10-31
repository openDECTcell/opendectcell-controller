// SPDX-License-Identifier: AGPL-3.0-or-later

package cdect

import (
	"errors"
	"testing"
)

// panicEnv panics in Send, the callback the host's install runs.
type panicEnv struct{ traceEnv }

func (e *panicEnv) Send(Mail) error { panic("send broke") }

// A panic in a callback comes back as a *Fault from the Host method that
// called into C, not through the C frames (libopendectcell-integration.md §8).
func TestCallbackPanicRaisedAfterC(t *testing.T) {
	e := &panicEnv{traceEnv{t: t}}
	x := NewHost(e, Config{PIN: "12345678"})
	defer x.Close()
	x.AddRecord(0, subRecord(1, []byte{0x00, 0x11, 0x22, 0x33, 0x44}))
	if err := x.Start(); err != nil {
		t.Fatal(err)
	}
	x.RadioUp(true)

	var got any
	func() {
		defer func() { got = recover() }()
		for now := uint64(1000); now < 3000; now += 10 {
			x.Tick(now)
		}
	}()
	var f *Fault
	if err, ok := got.(error); !ok || !errors.As(err, &f) {
		t.Fatalf("recovered %v, want a *Fault", got)
	}
	if f.Callback != "send" || f.Value != "send broke" {
		t.Errorf("fault %s: %v", f.Callback, f.Value)
	}
	x.Tick(3000) // the fault is raised once
}
