// SPDX-License-Identifier: AGPL-3.0-or-later

package logctl

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newLog(c *Controller) (*slog.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return slog.New(c.Handler(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: LevelCalls}))), &b
}

func TestPerComponentLevels(t *testing.T) {
	c := New(slog.LevelInfo)
	log, buf := newLog(c)
	sip, cells := log.With("component", "sip"), log.With("component", "cells")

	sip.Debug("sip-debug-1")
	sip.Info("sip-info-1")
	if err := c.Set("sip", "debug", 0); err != nil {
		t.Fatal(err)
	}
	sip.Debug("sip-debug-2")
	cells.Debug("cells-debug")
	if err := c.Set("cells", "off", 0); err != nil {
		t.Fatal(err)
	}
	cells.Error("cells-error")
	sip.Log(nil, LevelCalls, "sip-call-line") //nolint:staticcheck // nil ctx is fine for the test handler
	out := buf.String()
	for want, in := range map[string]bool{"sip-debug-1": false, "sip-info-1": true, "sip-debug-2": true, "cells-debug": false, "cells-error": false, "sip-call-line": false} {
		if strings.Contains(out, want) != in {
			t.Errorf("%s: logged=%v, want %v\n%s", want, !in, in, out)
		}
	}
	if err := c.Set("sip", "calls", 0); err != nil {
		t.Fatal(err)
	}
	sip.Log(nil, LevelCalls, "sip-call-line-2") //nolint:staticcheck // as above
	if !strings.Contains(buf.String(), "sip-call-line-2") {
		t.Error("calls level not logged")
	}
}

func TestDefaultAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	c := New(slog.LevelWarn)
	c.now = func() time.Time { return now }
	log, buf := newLog(c)
	web := log.With("component", "web")
	web.Info("a")
	if err := c.Set("web", "debug", time.Hour); err != nil {
		t.Fatal(err)
	}
	web.Debug("b")
	st := c.State()
	if st.Components["web"].Until == nil || st.Default != "warn" {
		t.Fatalf("state %+v", st)
	}
	if got := c.Expire(); len(got) != 0 {
		t.Fatalf("expired early: %v", got)
	}
	now = now.Add(2 * time.Hour)
	if got := c.Expire(); len(got) != 1 || got[0] != "web" {
		t.Fatalf("expired %v", got)
	}
	web.Debug("c")
	web.Warn("d")
	out := buf.String()
	if strings.Contains(out, "msg=a") || !strings.Contains(out, "msg=b") || strings.Contains(out, "msg=c") || !strings.Contains(out, "msg=d") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestApplySkipsExpiredAndUnknown(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	c := New(slog.LevelInfo)
	c.now = func() time.Time { return now }
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	c.Apply(State{Default: "error", Components: map[string]Setting{
		"sip": {Level: "debug", Until: &past}, "web": {Level: "debug", Until: &future},
		"nope": {Level: "debug"}, "dect": {Level: "bogus"},
	}})
	got := map[string]string{}
	for _, r := range c.Effective() {
		got[r.Component] = r.Level
	}
	if got["sip"] != "error" || got["web"] != "debug" || got["dect"] != "error" || got["core"] != "error" {
		t.Errorf("levels %v", got)
	}
	if err := c.Set("nope", "info", 0); err != ErrUnknownComponent {
		t.Errorf("unknown component: %v", err)
	}
	if err := c.Set("web", "loud", 0); err == nil {
		t.Error("bad level accepted")
	}
}
