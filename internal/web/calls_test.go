// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/media"
	"github.com/openDECTcell/opendectcell-controller/internal/media/rtpstats"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
)

type (
	sipStats = rtp.Stats
	cellRx   = rtpstats.Stats
)

// The admin call page shows each call's quality per leg, and says which leg
// is the worse one.
func TestAdminCalls(t *testing.T) {
	e := newEnv(t)
	admin := e.browser()
	e.login(t, admin, "admin", "admin password")
	if _, body := e.get(t, admin, "/admin/calls"); !strings.Contains(body, "No calls yet") {
		t.Error("empty call log")
	}

	s := &calllog.Store{Pool: e.store.Pool}
	start := time.Now().Add(-time.Hour)
	id, err := s.Start(ctx, calllog.Call{Direction: "out", RemoteParty: "210", StartedAt: start})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Answered(ctx, id, start.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	rtt, peer := 48.0, 2.5
	q := media.Summarise(rtpStats(), cellStats(), media.Stats{})
	q.SIP.RTTMs, q.SIP.PeerLossPct = &rtt, &peer
	q.WorseLeg = "cell"
	yes := true
	if err := s.Ended(ctx, id, calllog.End{At: start.Add(74 * time.Second), Reason: "normal", SIPCodec: "G722", CellCodec: "L16", SRTP: &yes, Quality: &q}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, calllog.Call{Direction: "in", RemoteParty: "+4930999", StartedAt: start.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}

	resp, body := e.get(t, admin, "/admin/calls")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if strings.Contains(body, "4930999") {
		t.Error("the admin call page shows a remote number in full")
	}
	for _, want := range []string{"1:12", "G722 · cell L16 · SRTP", "peer 2.5 %", "RTT 48.0 ms", "loss 10.0 %", "<strong>cell</strong>", "*****999", "not answered", "ongoing", "no media"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func rtpStats() (s sipStats) { return }

func cellStats() (s cellRx) {
	s.Received, s.Expected, s.Lost, s.LossRate = 900, 1000, 100, 0.1
	return
}
