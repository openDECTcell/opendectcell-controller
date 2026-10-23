// SPDX-License-Identifier: AGPL-3.0-or-later

// Package jitter is the media engine's adaptive jitter buffer
// (docs/media-engine.md §5). It reorders RTP packets, holds them for a
// target delay that follows the measured network jitter, and tells the
// caller when a packet is missing so it can conceal it.
//
// The buffer is driven by its consumer: the pipeline pops one packet's worth
// of audio whenever it needs more, on its own 20 ms clock, and passes the
// time. The buffer has no clock of its own, which makes it deterministic in
// tests.
//
// What it controls is the playout slack: how long after the fastest packets
// of the stream would have arrived the packet due now is played. A packet
// whose network delay exceeds the fastest by more than the slack is late.
//
//   - Target slack = 95th percentile of the delay variation over the last
//     ~4 s, clamped to [MinDelay, MaxDelay]. MinDelay is the least jitter
//     always absorbed.
//   - The slack grows by stretching: when it is short of the target, the
//     consumer conceals one frame without the playout point moving. Stretches are at least 100 ms apart. A loss does
//     not lower the slack, so it never causes a stretch.
//   - When the buffer runs dry, the consumer conceals without the playout
//     point moving, which also grows the slack. Dry for longer than MaxDelay
//     is a gap (silence suppression, an outage), not a loss: the buffer
//     stops concealing and refills to the target before playing again.
//   - The slack shrinks when it has exceeded the target by one and a half
//     frames for a whole second: one frame is skipped, at most once a second.
//   - At the start of a talk spurt (marker bit) into an empty buffer, and
//     after a gap longer than MaxDelay, the playout point moves to the new
//     packet and refills: silence absorbs the change.
//   - A packet whose timing is inconsistent with the stream's by more than
//     four times MaxDelay (a sender restart, a timestamp jump) starts the
//     measurement over.
package jitter

import (
	"slices"
	"time"
)

// Packet is a received RTP packet.
type Packet struct {
	Seq     uint16
	TS      uint32
	Marker  bool
	SSRC    uint32
	Payload []byte
	Arrival time.Time
}

// Config sets the buffer's parameters.
type Config struct {
	ClockRate int    // RTP clock, Hz
	Frame     uint32 // the consumer's frame, in timestamp units (20 ms)
	MinDelay  time.Duration
	MaxDelay  time.Duration
	// Duration of a packet in timestamp units, from its payload size.
	Duration func(payload []byte) uint32
}

// Defaults for the slack range (docs/media-engine.md §5).
const (
	DefaultMinDelay = 20 * time.Millisecond
	DefaultMaxDelay = 120 * time.Millisecond
)

// Status is the outcome of a Pop.
type Status int

const (
	// Waiting: nothing to play yet (before the buffer has filled to its
	// target, or in a gap). Play silence.
	Waiting Status = iota
	// OK: a packet to decode.
	OK
	// Lost: no packet to play; conceal the returned number of units.
	Lost
)

// Stats are the buffer's counters.
type Stats struct {
	Received  uint64 // packets accepted into the buffer
	Late      uint64 // arrived after their playout point
	Duplicate uint64
	Lost      uint64 // concealed because missing when due (late ones included)
	Underruns uint64 // concealed because the buffer was dry
	Stretched uint64 // concealed to grow the slack toward the target
	Skipped   uint64 // frames skipped to shrink the slack
	Resyncs   uint64
	Slack     time.Duration // current playout slack
	Target    time.Duration
	JitterP95 time.Duration // delay variation, 95th percentile
}

// Buffer is a jitter buffer for one RTP stream. It is not safe for
// concurrent use.
type Buffer struct {
	cfg     Config
	pkts    []Packet // ordered by TS
	started bool     // a stream is being measured
	playing bool     // the playout point is running
	next    uint32   // playout point, RTP timestamp
	ssrc    uint32
	// Timing reference: the first packet of the measurement.
	t0  time.Time
	ts0 uint32
	// Transit times (arrival − timestamp, in RTP units relative to the
	// reference) of recent packets, a ring.
	transit []int64
	tpos    int
	tfull   bool
	minTr   int64
	target  uint32 // slack, timestamp units
	over    int    // consecutive pops with the slack well above target
	sinceSk int    // pops since the last skip
	sinceSt int    // pops since the last stretch
	dry     uint32 // units concealed in the current underrun
	stats   Stats
}

// New makes a buffer.
func New(cfg Config) *Buffer {
	if cfg.MinDelay == 0 {
		cfg.MinDelay = DefaultMinDelay
	}
	if cfg.MaxDelay == 0 {
		cfg.MaxDelay = DefaultMaxDelay
	}
	window := 4 * cfg.ClockRate / int(cfg.Frame) // about 4 s of packets
	b := &Buffer{cfg: cfg, transit: make([]int64, window)}
	b.target = b.units(cfg.MinDelay)
	return b
}

func (b *Buffer) units(d time.Duration) uint32 {
	return uint32(d * time.Duration(b.cfg.ClockRate) / time.Second)
}

func (b *Buffer) dur(u int64) time.Duration {
	return time.Duration(u) * time.Second / time.Duration(b.cfg.ClockRate)
}

// diff is a − b in serial-number arithmetic (RFC 1982) on 32 bits.
func diff(a, b uint32) int32 { return int32(a - b) }

// at is time t in RTP units since the reference.
func (b *Buffer) at(t time.Time) int64 {
	return int64(t.Sub(b.t0)) * int64(b.cfg.ClockRate) / int64(time.Second)
}

func (b *Buffer) rel(ts uint32) int64 { return int64(diff(ts, b.ts0)) }

// Push adds a received packet.
func (b *Buffer) Push(p Packet) {
	if b.started && p.SSRC != b.ssrc {
		b.restart() // a new source
	}
	if !b.started {
		b.begin(p)
	}
	tr := b.at(p.Arrival) - b.rel(p.TS)
	if abs(tr-b.minTr) > 4*int64(b.units(b.cfg.MaxDelay)) {
		// The timestamp no longer fits the clock: a jump or a restart.
		b.restart()
		b.begin(p)
		tr = 0
	}
	b.measure(tr)
	end := p.TS + b.cfg.Duration(p.Payload)
	switch {
	case b.playing && diff(end, b.next) <= 0:
		b.stats.Late++
		return
	case b.playing && p.Marker && len(b.pkts) == 0 && p.TS != b.next:
		// A new talk spurt into an empty buffer: refill to the target.
		b.resync()
	case b.playing && diff(p.TS, b.next) > int32(b.units(b.cfg.MaxDelay)+b.target):
		// Far ahead of the playout point (a gap the sender did not mark):
		// waiting would only add delay.
		b.pkts = b.pkts[:0]
		b.resync()
	}
	i, found := slices.BinarySearchFunc(b.pkts, p.TS, func(q Packet, ts uint32) int { return int(diff(q.TS, ts)) })
	if found {
		b.stats.Duplicate++
		return
	}
	b.pkts = slices.Insert(b.pkts, i, p)
	b.stats.Received++
}

func abs(x int64) int64 { return max(x, -x) }

func (b *Buffer) begin(p Packet) {
	b.ssrc, b.started = p.SSRC, true
	b.t0, b.ts0 = p.Arrival, p.TS
	b.tpos, b.tfull, b.minTr = 0, false, 0
}

func (b *Buffer) restart() {
	b.pkts = b.pkts[:0]
	b.started, b.playing = false, false
	b.stats.Resyncs++
}

func (b *Buffer) resync() {
	b.playing = false
	b.stats.Resyncs++
}

// measure records a transit time and updates the target.
func (b *Buffer) measure(tr int64) {
	b.transit[b.tpos] = tr
	b.tpos++
	if b.tpos == len(b.transit) {
		b.tpos, b.tfull = 0, true
	}
	n := b.tpos
	if b.tfull {
		n = len(b.transit)
	}
	v := slices.Clone(b.transit[:n])
	slices.Sort(v)
	b.minTr = v[0]
	p95 := v[(n-1)*95/100] - v[0]
	b.stats.JitterP95 = b.dur(p95)
	b.target = min(max(uint32(p95), b.units(b.cfg.MinDelay)), b.units(b.cfg.MaxDelay))
}

// slack is how far the playout point lags the fastest arrivals, at now.
func (b *Buffer) slack(now time.Time) int64 {
	return b.at(now) - b.rel(b.next) - b.minTr
}

// Pop returns what to play at now. With OK, decode the packet; with Lost,
// conceal n timestamp units; with Waiting, play n units of silence.
func (b *Buffer) Pop(now time.Time) (p Packet, st Status, n uint32) {
	if !b.started {
		return Packet{}, Waiting, b.cfg.Frame
	}
	if !b.playing {
		if len(b.pkts) == 0 {
			return Packet{}, Waiting, b.cfg.Frame
		}
		b.next = b.pkts[0].TS
		if b.slack(now) < int64(b.target) {
			return Packet{}, Waiting, b.cfg.Frame
		}
		b.playing, b.dry = true, 0
	}
	b.dropBehind()
	b.sinceSt++
	b.sinceSk++
	if len(b.pkts) == 0 {
		// Dry: conceal without moving the playout point, until the gap is
		// too long to be a loss.
		b.dry += b.cfg.Frame
		if b.dry > b.units(b.cfg.MaxDelay) {
			b.playing = false
			return Packet{}, Waiting, b.cfg.Frame
		}
		b.stats.Underruns++
		return Packet{}, Lost, b.cfg.Frame
	}
	b.dry = 0
	s, t, f := b.slack(now), int64(b.target), int64(b.cfg.Frame)
	if s < t && b.sinceSt >= b.cfg.ClockRate/10/int(f) {
		b.sinceSt = 0
		b.stats.Stretched++
		return Packet{}, Lost, b.cfg.Frame
	}
	if s > t+f+f/2 {
		b.over++
	} else {
		b.over = 0
	}
	if pops := b.cfg.ClockRate / int(f); b.over >= pops && b.sinceSk >= pops {
		b.next += b.cfg.Frame
		b.dropBehind()
		b.over, b.sinceSk = 0, 0
		b.stats.Skipped++
		if len(b.pkts) == 0 {
			return Packet{}, Lost, b.cfg.Frame
		}
	}
	head := b.pkts[0]
	if d := diff(head.TS, b.next); d > 0 {
		// The packet due now is missing but later ones are here: it is lost
		// (or will be late). Conceal up to the next packet, a frame at most.
		n := min(uint32(d), b.cfg.Frame)
		b.next += n
		b.stats.Lost++
		return Packet{}, Lost, n
	}
	b.pkts = b.pkts[1:]
	n = b.cfg.Duration(head.Payload)
	b.next = head.TS + n
	return head, OK, n
}

// dropBehind discards packets that end at or before the playout point.
func (b *Buffer) dropBehind() {
	for len(b.pkts) > 0 && diff(b.pkts[0].TS+b.cfg.Duration(b.pkts[0].Payload), b.next) <= 0 {
		b.pkts = b.pkts[1:]
	}
}

// Stats returns the counters and the current delays, at now.
func (b *Buffer) Stats(now time.Time) Stats {
	s := b.stats
	if b.playing {
		s.Slack = b.dur(b.slack(now))
	}
	s.Target = b.dur(int64(b.target))
	return s
}
