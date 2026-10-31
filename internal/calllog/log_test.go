// SPDX-License-Identifier: AGPL-3.0-or-later

package calllog_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/logctl"
)

// Call events are written at the calls level only, with the number masked.
func TestCallsLevelLines(t *testing.T) {
	pool := dbtest.Migrated(t)
	ctl := logctl.New(slog.LevelInfo)
	var buf bytes.Buffer
	log := slog.New(ctl.Handler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: logctl.LevelCalls}))).With("component", "dect")
	s := &calllog.Store{Pool: pool, Log: log}

	id, err := s.Start(ctx, calllog.Call{Direction: "out", RemoteParty: "+4930123456", StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("logged at info: %s", buf.String())
	}
	if err := ctl.Set("dect", "calls", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Answered(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Ended(ctx, id, calllog.End{At: time.Now(), Reason: "hung up"}); err != nil {
		t.Fatal(err)
	}
	id2, _ := s.Start(ctx, calllog.Call{Direction: "in", RemoteParty: "+4930123456", StartedAt: time.Now()})
	out := buf.String()
	for _, want := range []string{"call answered", "call ended", "call started", "remote=*******456"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "4930123") {
		t.Errorf("number not masked:\n%s", out)
	}
	_ = id2
}
