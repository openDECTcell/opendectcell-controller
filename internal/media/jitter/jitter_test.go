// SPDX-License-Identifier: AGPL-3.0-or-later

package jitter

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

const frame = 160 // 20 ms at 8 kHz

var t0 = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func newBuf() *Buffer {
	return New(Config{ClockRate: 8000, Frame: frame, Duration: func(p []byte) uint32 { return uint32(len(p)) }})
}

// pkt is packet i of a 20 ms stream starting at timestamp 1000, arriving
// after delay.
func pkt(i int, delay time.Duration) Packet {
	return Packet{Seq: uint16(i), TS: uint32(1000 + i*frame), SSRC: 7, Payload: make([]byte, frame), Arrival: t0.Add(time.Duration(i)*20*time.Millisecond + delay)}
}

// result of a simulated stream
type result struct {
	ok, lost, waiting int
	order             []uint32 // timestamps played, in order
	stats             Stats
}

// run feeds pkts by arrival time and pops on a 20 ms clock for dur, the way
// the pipeline does: whenever less than a frame of audio is in hand.
func run(b *Buffer, pkts []Packet, dur time.Duration) result {
	slices.SortStableFunc(pkts, func(a, b Packet) int { return a.Arrival.Compare(b.Arrival) })
	var r result
	have := 0
	for tick := time.Duration(0); tick < dur; tick += 20 * time.Millisecond {
		now := t0.Add(tick)
		for len(pkts) > 0 && !pkts[0].Arrival.After(now) {
			b.Push(pkts[0])
			pkts = pkts[1:]
		}
		for have < frame {
			p, st, n := b.Pop(now)
			switch st {
			case OK:
				r.ok++
				r.order = append(r.order, p.TS)
			case Lost:
				r.lost++
			case Waiting:
				r.waiting++
			}
			have += int(n)
		}
		have -= frame
	}
	r.stats = b.Stats(t0.Add(dur))
	return r
}

func stream(n int, delay func(i int) time.Duration) []Packet {
	var ps []Packet
	for i := range n {
		ps = append(ps, pkt(i, delay(i)))
	}
	return ps
}

func inOrder(ts []uint32) bool {
	for i := 1; i < len(ts); i++ {
		if ts[i] != ts[i-1]+frame {
			return false
		}
	}
	return true
}

func TestSteadyStream(t *testing.T) {
	b := newBuf()
	r := run(b, stream(250, func(int) time.Duration { return 5 * time.Millisecond }), 5*time.Second)
	if r.lost != 0 || !inOrder(r.order) || r.ok < 240 {
		t.Errorf("steady stream: %d ok, %d lost, in order %v", r.ok, r.lost, inOrder(r.order))
	}
	if r.stats.Target != DefaultMinDelay || r.stats.Stretched != 0 || r.stats.Skipped != 0 {
		t.Errorf("no jitter, but the target is %v", r.stats.Target)
	}
	if r.waiting > 3 {
		t.Errorf("initial fill took %d frames", r.waiting)
	}
}

func TestReorder(t *testing.T) {
	b := newBuf()
	// Every other pair swapped, 10 ms apart.
	r := run(b, stream(250, func(i int) time.Duration {
		if i%4 == 2 {
			return 25 * time.Millisecond
		}
		return 0
	}), 5*time.Second)
	t.Logf("lost %d, %+v", r.lost, r.stats)
	// The first reordered packets come before the target has grown to cover
	// them; after that, nothing is lost.
	if r.stats.Lost > 3 || r.stats.Late > 3 {
		t.Errorf("reordered stream: %d lost, %d late", r.stats.Lost, r.stats.Late)
	}
	if !inOrder(r.order[20:]) {
		t.Error("reordered stream played out of order")
	}
}

func TestSingleLoss(t *testing.T) {
	b := newBuf()
	ps := stream(250, func(int) time.Duration { return 0 })
	ps = slices.Delete(ps, 100, 101)
	r := run(b, ps, 5*time.Second)
	if r.lost != 1 || r.stats.Lost != 1 || r.stats.Stretched != 0 {
		t.Errorf("one packet missing, %d concealed: %+v", r.lost, r.stats)
	}
	if slices.Contains(r.order, 1000+100*frame) {
		t.Error("the missing packet was played")
	}
}

// Under 0–60 ms of random jitter the target settles above the jitter and
// late packets become rare.
func TestAdaptsToJitter(t *testing.T) {
	b := newBuf()
	rng := rand.New(rand.NewPCG(1, 1)) //nolint:gosec // reproducible jitter
	r := run(b, stream(1500, func(int) time.Duration { return time.Duration(rng.IntN(60)) * time.Millisecond }), 30*time.Second)
	t.Logf("lost %d, %+v", r.lost, r.stats)
	if r.stats.Target < 50*time.Millisecond || r.stats.Target > DefaultMaxDelay {
		t.Errorf("target %v under 0–60 ms jitter", r.stats.Target)
	}
	if loss := float64(r.lost) / 1500; loss > 0.02 {
		t.Errorf("%.1f %% concealed under jitter the buffer can absorb", 100*loss)
	}
	if !inOrder(r.order[:len(r.order)/2]) && r.stats.Lost == 0 {
		t.Error("played out of order")
	}
}

// A one-second delay step grows the buffer (underrun), and it shrinks back
// once the step is over.
func TestDelaySpike(t *testing.T) {
	b := newBuf()
	r := run(b, stream(1000, func(i int) time.Duration {
		if i >= 200 && i < 250 {
			return 80 * time.Millisecond
		}
		return 0
	}), 20*time.Second)
	t.Logf("lost %d, %+v", r.lost, r.stats)
	if r.stats.Underruns+r.stats.Stretched == 0 {
		t.Error("an 80 ms delay step did not grow the buffer")
	}
	if r.stats.Skipped == 0 {
		t.Error("the buffer never shrank after the step")
	}
	if r.stats.Slack > r.stats.Target+30*time.Millisecond {
		t.Errorf("slack still %v long after the step (target %v)", r.stats.Slack, r.stats.Target)
	}
}

func TestLateAndDuplicate(t *testing.T) {
	b := newBuf()
	ps := stream(100, func(int) time.Duration { return 0 })
	late := ps[50]
	late.Arrival = late.Arrival.Add(300 * time.Millisecond)
	ps[50] = late
	ps = append(ps, ps[20]) // duplicate, same arrival
	r := run(b, ps, 3*time.Second)
	if r.stats.Late != 1 || r.stats.Duplicate != 1 {
		t.Errorf("late %d, duplicate %d; want 1 and 1", r.stats.Late, r.stats.Duplicate)
	}
}

// Silence suppression: the sender stops for two seconds and starts a new
// talk spurt with the marker bit. The buffer waits instead of conceal-
// ling two seconds, and starts the new spurt after its target delay.
func TestTalkSpurt(t *testing.T) {
	b := newBuf()
	var ps []Packet
	for i := range 400 {
		if i >= 100 && i < 200 {
			continue
		}
		p := pkt(i, 0)
		p.Marker = i == 200
		ps = append(ps, p)
	}
	r := run(b, ps, 9*time.Second)
	t.Logf("lost %d, %+v", r.lost, r.stats)
	// Until a gap exceeds MaxDelay it cannot be told from a loss, so up to
	// 120 ms is concealed (and fades to silence by 60 ms). The end of the
	// stream is a second gap.
	if r.lost > 2*int(DefaultMaxDelay/(20*time.Millisecond)) || r.stats.Lost != 0 {
		t.Errorf("concealed %d frames across a silence gap", r.lost)
	}
	if r.ok != 300 {
		t.Errorf("played %d of 300 packets", r.ok)
	}
}

func TestSSRCChange(t *testing.T) {
	b := newBuf()
	ps := stream(100, func(int) time.Duration { return 0 })
	for i := 50; i < 100; i++ {
		ps[i].SSRC = 8
		ps[i].TS += 999999 // a new source has its own timestamps
	}
	r := run(b, ps, 3*time.Second)
	if r.stats.Resyncs == 0 || r.ok < 95 {
		t.Errorf("SSRC change: %d resyncs, %d played", r.stats.Resyncs, r.ok)
	}
}

// Packets longer than the consumer's frame (30 ms against 20 ms) play out
// whole and in order.
func TestPacketSize(t *testing.T) {
	b := newBuf()
	var ps []Packet
	for i := range 100 {
		ps = append(ps, Packet{TS: uint32(i * 240), SSRC: 1, Payload: make([]byte, 240), Arrival: t0.Add(time.Duration(i) * 30 * time.Millisecond)})
	}
	r := run(b, ps, 4*time.Second)
	// Popping 30 ms packets on a 20 ms clock makes the slack swing by 10 ms,
	// which may cost a stretch at the start, but nothing is lost.
	if r.stats.Lost != 0 || r.stats.Late != 0 || r.ok != 100 || r.stats.Stretched > 1 {
		t.Errorf("30 ms packets: %d ok, %+v", r.ok, r.stats)
	}
	if !slices.IsSorted(r.order) {
		t.Error("30 ms packets out of order")
	}
}
