// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// trustRecords is a Records that can also drop what its owners do not trust; each run waits for a release.
type trustRecords struct {
	*fakeRecords
	runs    atomic.Int32
	release chan struct{}
}

func (r *trustRecords) DropUntrusted(context.Context) ([]Dropped, error) {
	r.runs.Add(1)
	<-r.release
	return nil, nil
}

// EnforceTrust returns at once, runs one clean-up at a time, and runs once more if it was asked while
// one was running (the request may be about a change that clean-up did not see).
func TestEnforceTrustRunsOneAtATimeAndOnceMore(t *testing.T) {
	recs := &trustRecords{fakeRecords: &fakeRecords{}, release: make(chan struct{})}
	c := &Core{Link: &fakeLink{}, Records: recs, Log: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)

	c.EnforceTrust()
	for recs.runs.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	for range 5 {
		c.EnforceTrust() // while the first is running
	}
	recs.release <- struct{}{} // the first run ends ...
	for recs.runs.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	recs.release <- struct{}{} // ... and the one more request makes a second run
	time.Sleep(50 * time.Millisecond)
	if n := recs.runs.Load(); n != 2 {
		t.Fatalf("%d clean-ups, want 2", n)
	}
	// and nothing is left running: a new request starts a run again
	c.EnforceTrust()
	for recs.runs.Load() < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	recs.release <- struct{}{}
	cancel()
	c.Wait()
}

// A Records that cannot clean up (no trust settings) makes EnforceTrust do nothing.
func TestEnforceTrustWithoutAStoreThatCanDoIt(t *testing.T) {
	c := &Core{Link: &fakeLink{}, Records: &fakeRecords{}, Log: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	c.EnforceTrust()
	cancel()
	c.Wait()
}
