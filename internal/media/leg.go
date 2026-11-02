// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	prtp "github.com/pion/rtp"
	"github.com/pion/srtp/v3"

	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/media/rtpstats"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
)

// Packet is a received media packet, on either leg.
type Packet struct {
	PT      uint8
	Seq     uint16
	TS      uint32
	SSRC    uint32
	Marker  bool
	Payload []byte
}

// Leg is one side of a call's media: the SIP endpoint or the cell.
type Leg interface {
	// ReadPacket returns the next media packet, blocking until deadline.
	ReadPacket(deadline time.Time) (Packet, error)
	// WritePacket sends a packet and then advances the RTP timestamp by
	// units.
	WritePacket(pt uint8, payload []byte, units uint32, marker bool) error
	// Skip advances the RTP timestamp without sending, for time in which
	// nothing is sent.
	Skip(units uint32)
}

// SIPLeg adapts a SIP media endpoint.
func SIPLeg(e *rtp.Endpoint) Leg { return sipLeg{e} }

type sipLeg struct{ e *rtp.Endpoint }

// ReadPacket returns the next RTP packet of the SIP endpoint, waiting until deadline.
func (l sipLeg) ReadPacket(deadline time.Time) (Packet, error) {
	p, err := l.e.Read(deadline)
	return Packet{PT: p.PT, Seq: p.Seq, TS: p.Timestamp, SSRC: p.SSRC, Marker: p.Marker, Payload: p.Payload}, err
}

// WritePacket sends one RTP packet and advances the endpoint's timestamp by units
// (RTP clock ticks).
func (l sipLeg) WritePacket(pt uint8, payload []byte, units uint32, marker bool) error {
	return l.e.Write(pt, payload, units, marker)
}

// Skip advances the endpoint's timestamp by units without sending.
func (l sipLeg) Skip(units uint32) { l.e.Skip(units) }

// CellLeg is the controller's end of one media flow with a cell
// (cell-protocol.md §8.1, §8.3): RTP or SRTP over UDP, keyed and identified
// by the MEDIA_OPEN the controller sent. It sends only to the address of the
// most recent packet that authenticated (SRTP) or carried the expected SSRC
// (plain RTP), and nothing before the first one.
type CellLeg struct {
	conn     *net.UDPConn
	ssrcUp   uint32
	ssrcDown uint32
	send     *srtp.Context // nil for plain RTP
	recv     *srtp.Context
	rx       *rtpstats.Receiver

	mu     sync.Mutex
	remote netip.AddrPort
	seq    uint16
	ts     uint32
	closed bool
}

// NewMediaOpen prepares a MEDIA_OPEN for a plan: random SSRCs and, unless
// profile is wire.SRTPNone, fresh random keys for each direction
// (cell-protocol.md §8.1: never reused with the same SSRC).
func NewMediaOpen(mediaID uint32, channel uint8, plan Plan, pt uint8, ctl netip.AddrPort, profile uint8) (wire.MediaOpenMsg, error) {
	m := wire.MediaOpenMsg{MediaID: mediaID, AudioChannel: channel, Format: plan.CellDown, FormatUp: plan.CellUp,
		PayloadType: pt, CtlAddr: ctl, SRTPProfile: profile}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return m, err
	}
	m.SSRCUp, m.SSRCDown = binary.BigEndian.Uint32(b[:4]), binary.BigEndian.Uint32(b[4:])
	if m.SSRCUp == m.SSRCDown {
		m.SSRCDown++
	}
	if profile != wire.SRTPNone {
		n := wire.SRTPKeyLen(profile)
		if n == 0 {
			return m, fmt.Errorf("media: unknown SRTP profile %d", profile)
		}
		m.KeyUp, m.KeyDown = make([]byte, n), make([]byte, n)
		if _, err := rand.Read(m.KeyUp); err != nil {
			return m, err
		}
		if _, err := rand.Read(m.KeyDown); err != nil {
			return m, err
		}
	}
	return m, nil
}

func cellSRTP(profile uint8, key []byte) (*srtp.Context, error) {
	switch profile {
	case wire.SRTPAES128SHA80:
		return srtp.CreateContext(key[:16], key[16:], srtp.ProtectionProfileAes128CmHmacSha1_80)
	case wire.SRTPAEAD128GCM:
		return srtp.CreateContext(key[:16], key[16:], srtp.ProtectionProfileAeadAes128Gcm)
	}
	return nil, fmt.Errorf("media: unknown SRTP profile %d", profile)
}

// NewCellLeg makes the controller's end of the flow that m opens, on conn
// (bound to m.CtlAddr). The leg owns conn from now on.
func NewCellLeg(conn *net.UDPConn, m wire.MediaOpenMsg, clockRate int) (*CellLeg, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	l := &CellLeg{conn: conn, ssrcUp: m.SSRCUp, ssrcDown: m.SSRCDown, rx: rtpstats.NewReceiver(clockRate),
		seq: binary.BigEndian.Uint16(b[:2]), ts: binary.BigEndian.Uint32(b[2:])}
	if m.SRTPProfile != wire.SRTPNone {
		if len(m.KeyUp) != wire.SRTPKeyLen(m.SRTPProfile) || len(m.KeyDown) != len(m.KeyUp) {
			return nil, errors.New("media: SRTP key has the wrong length")
		}
		var err error
		if l.send, err = cellSRTP(m.SRTPProfile, m.KeyDown); err != nil {
			return nil, err
		}
		if l.recv, err = cellSRTP(m.SRTPProfile, m.KeyUp); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// ReadPacket returns the next packet from the cell. Packets that fail
// authentication or carry another SSRC are dropped.
func (l *CellLeg) ReadPacket(deadline time.Time) (Packet, error) {
	buf := make([]byte, 1500)
	for {
		if err := l.conn.SetReadDeadline(deadline); err != nil {
			return Packet{}, err
		}
		n, from, err := l.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return Packet{}, err
		}
		b := buf[:n]
		if l.recv != nil {
			if b, err = l.recv.DecryptRTP(nil, b, nil); err != nil {
				continue
			}
		}
		var p prtp.Packet
		if err := p.Unmarshal(b); err != nil || p.Version != 2 || p.SSRC != l.ssrcUp {
			continue
		}
		l.mu.Lock()
		l.remote = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		l.mu.Unlock()
		l.rx.Update(p.SSRC, p.SequenceNumber, p.Timestamp, time.Now())
		return Packet{PT: p.PayloadType, Seq: p.SequenceNumber, TS: p.Timestamp, SSRC: p.SSRC, Marker: p.Marker, Payload: p.Payload}, nil
	}
}

// WritePacket sends to the latched cell address, or drops the packet before
// the cell's first valid packet.
func (l *CellLeg) WritePacket(pt uint8, payload []byte, units uint32, marker bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return net.ErrClosed
	}
	ts := l.ts
	l.ts += units
	if !l.remote.IsValid() {
		return nil
	}
	pkt := prtp.Packet{Header: prtp.Header{Version: 2, PayloadType: pt, SequenceNumber: l.seq, Timestamp: ts, SSRC: l.ssrcDown, Marker: marker}, Payload: payload}
	l.seq++
	b, err := pkt.Marshal()
	if err != nil {
		return err
	}
	if l.send != nil {
		if b, err = l.send.EncryptRTP(nil, b, nil); err != nil {
			return err
		}
	}
	_, err = l.conn.WriteToUDPAddrPort(b, l.remote)
	return err
}

// Skip advances the timestamp.
func (l *CellLeg) Skip(units uint32) {
	l.mu.Lock()
	l.ts += units
	l.mu.Unlock()
}

// Remote is the latched cell address, invalid before the first packet.
func (l *CellLeg) Remote() netip.AddrPort {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.remote
}

// Stats are the uplink's reception statistics (the cell link has no RTCP
// in v1, cell-protocol.md §8.3).
func (l *CellLeg) Stats() rtpstats.Stats { return l.rx.Stats() }

// Close releases the socket.
func (l *CellLeg) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return l.conn.Close()
}
