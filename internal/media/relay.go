// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// NewRelay makes the two ends of an in-process media path: what one end writes the other reads, as
// packets with the sequence numbers and timestamps of the writer. Two pipelines, each between a cell
// and one end, carry a call between two handsets (internal calls): every direction runs through its
// own jitter buffer and converter, so each handset gets the codec and rate it uses.
func NewRelay() (a, b Leg) {
	x, y := newRelayLeg(), newRelayLeg()
	x.peer, y.peer = y, x
	return x, y
}

const relayDepth = 32 // packets waiting for a reader (640 ms): more means the reader is gone

type relayLeg struct {
	peer *relayLeg
	in   chan Packet
	done chan struct{}
	once sync.Once

	mu   sync.Mutex
	seq  uint16
	ts   uint32
	ssrc uint32
}

var relaySSRC atomic.Uint32 // every end its own

func newRelayLeg() *relayLeg {
	return &relayLeg{in: make(chan Packet, relayDepth), done: make(chan struct{}), ssrc: relaySSRC.Add(1)}
}

// ReadPacket returns the next packet the other end wrote, os.ErrDeadlineExceeded at the deadline.
func (l *relayLeg) ReadPacket(deadline time.Time) (Packet, error) {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case p := <-l.in:
		return p, nil
	case <-l.done:
		return Packet{}, net.ErrClosed
	case <-t.C:
		return Packet{}, os.ErrDeadlineExceeded
	}
}

// WritePacket hands a packet to the other end; one nobody reads is dropped, as the network would.
func (l *relayLeg) WritePacket(pt uint8, payload []byte, units uint32, marker bool) error {
	l.mu.Lock()
	p := Packet{PT: pt, Seq: l.seq, TS: l.ts, SSRC: l.ssrc, Marker: marker, Payload: append([]byte(nil), payload...)}
	l.seq++
	l.ts += units
	l.mu.Unlock()
	select {
	case <-l.done:
		return net.ErrClosed
	default:
	}
	select {
	case l.peer.in <- p:
	default:
	}
	return nil
}

// Skip advances the timestamp for time in which nothing was sent.
func (l *relayLeg) Skip(units uint32) {
	l.mu.Lock()
	l.ts += units
	l.mu.Unlock()
}

// CloseRelay ends a relay end: its reads and writes fail from now on. The other end sees the same
// when its peer is closed too; closing one is enough to stop a pipeline that reads it.
func CloseRelay(l Leg) {
	if r, ok := l.(*relayLeg); ok {
		r.once.Do(func() { close(r.done) })
	}
}
