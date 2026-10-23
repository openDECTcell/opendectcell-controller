// SPDX-License-Identifier: AGPL-3.0-or-later

// Package rtpstats keeps RFC 3550 statistics for one RTP stream: the
// receiver side (sequence tracking, loss and interarrival jitter, appendix
// A.1, A.3 and A.8), the sender side (packet and octet counts for sender
// reports) and the round-trip time from the peer's reception reports
// (§6.4.1). It is used on both legs: the SIP leg exchanges RTCP with it, and
// the cell leg, which has no RTCP in v1 (cell-protocol.md §8.3), uses the
// receiver side alone.
package rtpstats

import (
	"math"
	"sync"
	"time"

	"github.com/pion/rtcp"
)

// RFC 3550 A.1 parameters.
const (
	maxDropout    = 3000
	maxMisorder   = 100
	minSequential = 2
)

// Receiver tracks one incoming stream. It is safe for concurrent use.
type Receiver struct {
	mu        sync.Mutex
	clockRate int
	// The peer's last sender report: its NTP timestamp (middle 32 bits) and
	// when we received it. It survives a change of stream.
	lsr     uint32
	lsrTime time.Time
	stream
}

// stream is the state of one SSRC's sequence.
type stream struct {
	ssrc      uint32
	init      bool
	probation int
	maxSeq    uint16
	cycles    uint32
	baseSeq   uint32
	badSeq    uint32
	received  uint64
	// For the fraction lost in each report interval.
	expectedPrior uint64
	receivedPrior uint64
	// Interarrival jitter, in timestamp units, A.8.
	transit int64
	jitter  float64
	haveTr  bool
	t0      time.Time
}

// NewReceiver tracks a stream with the given RTP clock rate.
func NewReceiver(clockRate int) *Receiver {
	return &Receiver{clockRate: clockRate}
}

// Update records a received packet.
func (r *Receiver) Update(ssrc uint32, seq uint16, ts uint32, arrival time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.init || ssrc != r.ssrc {
		r.reset(ssrc, seq, arrival)
	}
	if !r.updateSeq(seq) {
		return
	}
	// A.8: J += (|D| − J) / 16, D the change in transit time.
	arr := int64(arrival.Sub(r.t0)) * int64(r.clockRate) / int64(time.Second)
	tr := arr - int64(ts)
	if r.haveTr {
		d := float64(tr - r.transit)
		r.jitter += (math.Abs(d) - r.jitter) / 16
	}
	r.transit, r.haveTr = tr, true
}

func (r *Receiver) reset(ssrc uint32, seq uint16, arrival time.Time) {
	r.stream = stream{ssrc: ssrc, init: true, probation: minSequential, t0: arrival}
	r.initSeq(seq)
	r.maxSeq = seq - 1
}

func (r *Receiver) initSeq(seq uint16) {
	r.baseSeq = uint32(seq)
	r.maxSeq = seq
	r.badSeq = 1 << 16 // so seq == badSeq is false
	r.cycles = 0
	r.received, r.receivedPrior, r.expectedPrior = 0, 0, 0
}

// updateSeq is RFC 3550 A.1 update_seq; false means drop the packet.
func (r *Receiver) updateSeq(seq uint16) bool {
	udelta := seq - r.maxSeq
	if r.probation > 0 {
		if seq == r.maxSeq+1 {
			r.probation--
			r.maxSeq = seq
			if r.probation == 0 {
				r.initSeq(seq)
				r.received++
				return true
			}
		} else {
			r.probation = minSequential - 1
			r.maxSeq = seq
		}
		return false
	}
	switch {
	case udelta < maxDropout:
		if seq < r.maxSeq {
			r.cycles += 1 << 16
		}
		r.maxSeq = seq
	case udelta <= 1<<16-maxMisorder:
		// A large jump: accept it as a restart only if the next packet
		// follows it.
		if uint32(seq) == r.badSeq {
			r.initSeq(seq)
		} else {
			r.badSeq = uint32(seq+1) & 0xFFFF
			return false
		}
	default:
		// Duplicate or reordered: counted, not used for the sequence.
	}
	r.received++
	return true
}

// SenderReport records a sender report from the peer, for LSR/DLSR.
func (r *Receiver) SenderReport(ntp uint64, at time.Time) {
	r.mu.Lock()
	r.lsr, r.lsrTime = uint32(ntp>>16), at
	r.mu.Unlock()
}

// Stats are a stream's reception statistics.
type Stats struct {
	SSRC     uint32
	Received uint64
	Expected uint64
	Lost     int64 // cumulative; negative with duplicates
	// LossRate is Lost / Expected, 0..1.
	LossRate float64
	Jitter   time.Duration
}

// Stats returns the statistics so far.
func (r *Receiver) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statsLocked()
}

func (r *Receiver) expected() uint64 {
	if !r.init || r.received == 0 {
		return 0
	}
	return uint64(r.cycles) + uint64(r.maxSeq) - uint64(r.baseSeq) + 1
}

func (r *Receiver) statsLocked() Stats {
	s := Stats{SSRC: r.ssrc, Received: r.received, Expected: r.expected()}
	s.Lost = int64(s.Expected) - int64(s.Received)
	if s.Expected > 0 {
		s.LossRate = max(0, float64(s.Lost)/float64(s.Expected))
	}
	s.Jitter = time.Duration(r.jitter * float64(time.Second) / float64(r.clockRate))
	return s
}

// Report makes a reception report block for an RTCP SR or RR and starts a
// new interval for the fraction lost. ok is false before any packet.
func (r *Receiver) Report(now time.Time) (rr rtcp.ReceptionReport, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.init || r.received == 0 {
		return rr, false
	}
	exp := r.expected()
	lost := int64(exp) - int64(r.received)
	expInt := exp - r.expectedPrior
	recInt := r.received - r.receivedPrior
	r.expectedPrior, r.receivedPrior = exp, r.received
	var fraction uint8
	if lostInt := int64(expInt) - int64(recInt); expInt > 0 && lostInt > 0 {
		fraction = uint8(lostInt << 8 / int64(expInt))
	}
	rr = rtcp.ReceptionReport{
		SSRC:               r.ssrc,
		FractionLost:       fraction,
		TotalLost:          uint32(max(min(lost, 0x7FFFFF), -0x800000)) & 0xFFFFFF,
		LastSequenceNumber: r.cycles + uint32(r.maxSeq),
		Jitter:             uint32(r.jitter),
		LastSenderReport:   r.lsr,
	}
	if r.lsr != 0 {
		rr.Delay = uint32(now.Sub(r.lsrTime) * 65536 / time.Second)
	}
	return rr, true
}

// Sender counts what a stream has sent, for sender reports.
type Sender struct {
	mu      sync.Mutex
	packets uint32
	octets  uint32
	lastTS  uint32
	lastAt  time.Time
}

// Sent records a sent packet: its RTP timestamp and payload size.
func (s *Sender) Sent(ts uint32, payload int, at time.Time) {
	s.mu.Lock()
	s.packets++
	s.octets += uint32(payload)
	s.lastTS, s.lastAt = ts, at
	s.mu.Unlock()
}

// Report makes a sender report for ssrc at now, extrapolating the RTP
// timestamp from the last packet sent. ok is false before any packet.
func (s *Sender) Report(ssrc uint32, clockRate int, now time.Time) (sr rtcp.SenderReport, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.packets == 0 {
		return sr, false
	}
	ts := s.lastTS + uint32(now.Sub(s.lastAt)*time.Duration(clockRate)/time.Second)
	return rtcp.SenderReport{SSRC: ssrc, NTPTime: NTP(now), RTPTime: ts, PacketCount: s.packets, OctetCount: s.octets}, true
}

// ntpEpoch is 1900-01-01, the NTP era 0 origin.
var ntpEpoch = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)

// NTP is t as a 64-bit NTP timestamp.
func NTP(t time.Time) uint64 {
	d := t.Sub(ntpEpoch) // fits: era 0 ends in 2036
	sec := uint64(d / time.Second)
	frac := uint64(d%time.Second) << 32 / uint64(time.Second)
	return sec<<32 | frac
}

// RTT computes the round-trip time from a reception report the peer sent
// about our stream (RFC 3550 §6.4.1), received at now. ok is false when the
// report carries no LSR (we have sent no sender report yet).
func RTT(rr rtcp.ReceptionReport, now time.Time) (time.Duration, bool) {
	if rr.LastSenderReport == 0 {
		return 0, false
	}
	a := uint32(NTP(now) >> 16)
	d := int64(int32(a - rr.LastSenderReport - rr.Delay))
	if d < 0 {
		return 0, false
	}
	return time.Duration(d * int64(time.Second) / 65536), true
}

// PeerReport is what the peer said about our stream in its last report.
type PeerReport struct {
	At       time.Time
	LossRate float64 // fraction lost in the peer's last interval, 0..1
	Lost     int64   // cumulative
	Jitter   time.Duration
	RTT      time.Duration // 0 when not measurable
}

// FromReception turns a reception report about our stream into a
// PeerReport.
func FromReception(rr rtcp.ReceptionReport, clockRate int, now time.Time) PeerReport {
	lost := int64(rr.TotalLost)
	if lost&0x800000 != 0 {
		lost -= 1 << 24
	}
	p := PeerReport{At: now, LossRate: float64(rr.FractionLost) / 256, Lost: lost,
		Jitter: time.Duration(int64(rr.Jitter) * int64(time.Second) / int64(clockRate))}
	p.RTT, _ = RTT(rr, now)
	return p
}
