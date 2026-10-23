// SPDX-License-Identifier: AGPL-3.0-or-later

package rtpstats

import (
	"math"
	"testing"
	"time"

	"github.com/pion/rtcp"
)

var t0 = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// feed sends n packets of a 20 ms stream from seq0, skipping those for
// which skip is true, with arrival offsets from jitter.
func feed(r *Receiver, seq0 uint16, n int, skip func(i int) bool, jitter func(i int) time.Duration) {
	for i := 0; i < n; i++ {
		if skip != nil && skip(i) {
			continue
		}
		var j time.Duration
		if jitter != nil {
			j = jitter(i)
		}
		r.Update(7, seq0+uint16(i), uint32(5000+i*160), t0.Add(time.Duration(i)*20*time.Millisecond+j))
	}
}

func TestNoLoss(t *testing.T) {
	r := NewReceiver(8000)
	feed(r, 100, 500, nil, nil)
	s := r.Stats()
	// The first packet is on probation (RFC 3550 A.1) and not counted.
	if s.Received != 499 || s.Lost != 0 || s.Jitter != 0 {
		t.Errorf("clean stream: %+v", s)
	}
}

func TestLoss(t *testing.T) {
	r := NewReceiver(8000)
	feed(r, 100, 1000, func(i int) bool { return i > 10 && i%100 == 0 }, nil)
	s := r.Stats()
	if s.Lost != 9 || math.Abs(s.LossRate-9.0/999) > 1e-9 {
		t.Errorf("9 of 999 lost: %+v", s)
	}
}

func TestSequenceWrap(t *testing.T) {
	r := NewReceiver(8000)
	feed(r, 65000, 2000, nil, nil)
	s := r.Stats()
	if s.Lost != 0 || s.Received != 1999 {
		t.Errorf("across the wrap: %+v", s)
	}
	rr, ok := r.Report(t0)
	// One cycle (1<<16) plus the wrapped number is 65000+1999.
	if !ok || rr.LastSequenceNumber != 65000+1999 {
		t.Errorf("extended highest sequence %d", rr.LastSequenceNumber)
	}
}

func TestReorderAndDuplicate(t *testing.T) {
	r := NewReceiver(8000)
	feed(r, 0, 10, nil, nil)
	r.Update(7, 11, 5000+11*160, t0.Add(220*time.Millisecond))
	r.Update(7, 10, 5000+10*160, t0.Add(225*time.Millisecond))
	r.Update(7, 10, 5000+10*160, t0.Add(226*time.Millisecond)) // duplicate
	s := r.Stats()
	if s.Expected != 11 || s.Received != 12 || s.Lost != -1 || s.LossRate != 0 {
		t.Errorf("reorder + duplicate: %+v", s)
	}
}

// A.8 jitter is the mean absolute change in transit time: arrivals that
// alternate 10 ms early and late change by 20 ms every packet.
func TestJitter(t *testing.T) {
	r := NewReceiver(8000)
	feed(r, 0, 500, nil, func(i int) time.Duration {
		if i%2 == 0 {
			return 10 * time.Millisecond
		}
		return 0
	})
	if j := r.Stats().Jitter; j < 9*time.Millisecond || j > 11*time.Millisecond {
		t.Errorf("jitter %v for ±5 ms around 5 ms (|D| = 10 ms)", j)
	}
}

func TestReportFraction(t *testing.T) {
	r := NewReceiver(8000)
	feed(r, 0, 101, nil, nil)
	if rr, _ := r.Report(t0); rr.FractionLost != 0 {
		t.Errorf("first interval fraction %d", rr.FractionLost)
	}
	// Next interval: 100 expected, 25 lost.
	for i := 101; i < 201; i++ {
		if (i-101)%4 != 0 { // 101, 105, … 197 lost; 200 arrives
			r.Update(7, uint16(i), uint32(i*160), t0.Add(time.Duration(i)*20*time.Millisecond))
		}
	}
	rr, _ := r.Report(t0)
	if rr.FractionLost != 64 || rr.TotalLost != 25 {
		t.Errorf("25 %% loss reported as fraction %d, total %d", rr.FractionLost, rr.TotalLost)
	}
}

func TestRoundTrip(t *testing.T) {
	var s Sender
	s.Sent(1000, 160, t0)
	sr, ok := s.Report(42, 8000, t0.Add(500*time.Millisecond))
	if !ok || sr.PacketCount != 1 || sr.OctetCount != 160 || sr.RTPTime != 1000+4000 {
		t.Fatalf("sender report %+v", sr)
	}
	// The peer receives the SR, holds it 1 s, reports; we receive the report
	// 100 ms of network time after the SR left.
	sent := t0.Add(500 * time.Millisecond)
	peer := NewReceiver(8000)
	peer.SenderReport(sr.NTPTime, sent.Add(30*time.Millisecond))
	feed(peer, 0, 3, nil, nil)
	rr, _ := peer.Report(sent.Add(1030 * time.Millisecond))
	if rr.LastSenderReport != uint32(sr.NTPTime>>16) || rr.Delay != 65536 {
		t.Fatalf("LSR %#x, DLSR %d", rr.LastSenderReport, rr.Delay)
	}
	rtt, ok := RTT(rr, sent.Add(1100*time.Millisecond))
	if !ok || rtt < 99*time.Millisecond || rtt > 101*time.Millisecond {
		t.Errorf("RTT %v, want 100 ms", rtt)
	}
	if _, ok := RTT(rtcp.ReceptionReport{}, t0); ok {
		t.Error("RTT from a report without LSR")
	}
	p := FromReception(rtcp.ReceptionReport{FractionLost: 128, TotalLost: 0xFFFFFF, Jitter: 80}, 8000, t0)
	if p.LossRate != 0.5 || p.Lost != -1 || p.Jitter != 10*time.Millisecond {
		t.Errorf("FromReception %+v", p)
	}
}

func TestNTP(t *testing.T) {
	n := NTP(time.Date(1900, 1, 1, 0, 0, 1, int(time.Second/2), time.UTC))
	if n != 1<<32|1<<31 {
		t.Errorf("NTP(1.5 s) = %#x", n)
	}
}
