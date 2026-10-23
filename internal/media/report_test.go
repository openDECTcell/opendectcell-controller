// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/media/rtpstats"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
)

// Reference points worked through G.107 by hand: a clean call has R = 93.2,
// MOS 4.41; 5 % loss with Bpl = 25.1 gives Ie,eff = 95·5/30.1 = 15.8, so at
// 100 ms (Id = 2.4) R = 75.0 and MOS 3.82; 50 % loss gives R = 30, MOS 1.61.
func TestEModel(t *testing.T) {
	for _, c := range []struct {
		loss     float64
		delay    time.Duration
		min, max float64
	}{
		{0, 0, 4.39, 4.42},
		{0, 100 * time.Millisecond, 4.3, 4.37},
		{5, 100 * time.Millisecond, 3.81, 3.83},
		{0, 400 * time.Millisecond, 2.8, 3.5},
		{50, 0, 1.6, 1.62},
	} {
		if m := EModelMOS(c.loss, c.delay); m < c.min || m > c.max {
			t.Errorf("loss %.0f %%, delay %v: MOS %.2f, want %.2f–%.2f", c.loss, c.delay, m, c.min, c.max)
		}
	}
	for l := 0.0; l < 100; l++ {
		if EModelMOS(l+1, 0) > EModelMOS(l, 0) {
			t.Fatalf("MOS rises with loss at %.0f %%", l)
		}
	}
}

func TestSummarise(t *testing.T) {
	sip := rtp.Stats{
		Received: rtpstats.Stats{Received: 990, Expected: 1000, Lost: 10, LossRate: 0.01, Jitter: 12 * time.Millisecond},
		Peer:     rtpstats.PeerReport{At: time.Now(), LossRate: 0.02, Jitter: 8 * time.Millisecond, RTT: 60 * time.Millisecond},
	}
	cell := rtpstats.Stats{Received: 1000, Expected: 1000, Jitter: 2 * time.Millisecond}
	q := Summarise(sip, cell, Stats{Plan: nbPlan})
	if q.SIP.LossPct != 1 || *q.SIP.PeerLossPct != 2 || *q.SIP.RTTMs != 60 || q.Cell.LossPct != 0 || q.Cell.JitterMs != 2 {
		t.Errorf("%+v", q)
	}
	if q.WorseLeg != "sip" {
		t.Errorf("worse leg %q, want sip", q.WorseLeg)
	}
	if q.MOS < 3.5 || q.MOS > 4.4 {
		t.Errorf("MOS %.2f for 2 %% loss", q.MOS)
	}
	// The cell leg can be the bad one.
	q = Summarise(rtp.Stats{}, rtpstats.Stats{Received: 900, Expected: 1000, Lost: 100, LossRate: 0.1}, Stats{Plan: nbPlan})
	if q.WorseLeg != "cell" || q.SIP.PeerLossPct != nil || q.SIP.RTTMs != nil {
		t.Errorf("%+v", q)
	}
	// Clean on both: nobody to blame.
	if q = Summarise(rtp.Stats{}, rtpstats.Stats{}, Stats{Plan: nbPlan}); q.WorseLeg != "" || math.Abs(q.MOS-4.33) > 0.05 {
		t.Errorf("clean call: %+v", q)
	}
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	var back Quality
	if err := json.Unmarshal(b, &back); err != nil || back.MOS != q.MOS {
		t.Errorf("JSON round trip: %s", b)
	}
}
