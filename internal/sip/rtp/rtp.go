// SPDX-License-Identifier: AGPL-3.0-or-later

// Package rtp is the SIP side's media endpoint: one RTP/SRTP socket pair per
// call, symmetric RTP (RFC 4961), SDES-keyed SRTP and RFC 4733 DTMF events
// (docs/sip-ua.md §1). The media engine (C5) reads and writes payloads.
//
// RTCP (RFC 3550 §6) runs on the odd port beside the RTP port: the endpoint
// sends a sender or receiver report every 5 s on average and reads the
// peer's, which gives the call-quality statistics (Stats).
package rtp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/rtcp"
	prtp "github.com/pion/rtp"
	"github.com/pion/srtp/v3"

	"github.com/openDECTcell/opendectcell-controller/internal/media/rtpstats"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

// Ports hands out RTP/RTCP port pairs (even RTP port, RTCP = RTP + 1) from a
// range.
type Ports struct {
	IP       netip.Addr // address to bind; unspecified binds all
	Min, Max int

	mu   sync.Mutex
	next int
	used map[int]bool
}

// ErrNoPorts means the range is exhausted.
var ErrNoPorts = errors.New("rtp: no free port pair in the range")

func (p *Ports) alloc(ctx context.Context) (*net.UDPConn, *net.UDPConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used == nil {
		p.used = map[int]bool{}
	}
	lo := p.Min + p.Min%2
	if p.next < lo || p.next+1 > p.Max {
		p.next = lo
	}
	lc := net.ListenConfig{}
	for tries := 0; tries <= (p.Max-lo)/2; tries++ {
		port := p.next
		p.next += 2
		if p.next+1 > p.Max {
			p.next = lo
		}
		if p.used[port] {
			continue
		}
		a, err := lc.ListenPacket(ctx, "udp", netip.AddrPortFrom(p.IP, uint16(port)).String())
		if err != nil {
			continue
		}
		b, err := lc.ListenPacket(ctx, "udp", netip.AddrPortFrom(p.IP, uint16(port+1)).String())
		if err != nil {
			a.Close()
			continue
		}
		p.used[port] = true
		return a.(*net.UDPConn), b.(*net.UDPConn), nil
	}
	return nil, nil, ErrNoPorts
}

func (p *Ports) release(port int) {
	p.mu.Lock()
	delete(p.used, port)
	p.mu.Unlock()
}

// Endpoint is one call's media socket pair.
type Endpoint struct {
	ports     *Ports
	rtp, rtcp *net.UDPConn
	localPort int
	mu        sync.Mutex
	remote    netip.AddrPort
	sdpRemote netip.AddrPort
	latched   bool
	sendCtx   *srtp.Context
	recvCtx   *srtp.Context
	// recvMu serialises recvCtx between the RTP and RTCP readers: an SRTP
	// context is not safe for concurrent use.
	recvMu     sync.Mutex
	rtcpRemote netip.AddrPort // latched from the peer's RTCP; else remote port + 1
	rx         *rtpstats.Receiver
	tx         rtpstats.Sender
	peer       rtpstats.PeerReport
	cname      string
	done       chan struct{}
	secure     bool
	ssrc       uint32
	seq        uint16
	ts         uint32
	telEventPT uint8
	closed     bool
	// OnDTMF, if set, receives each RFC 4733 event once, at its end.
	OnDTMF  func(digit rune, duration time.Duration)
	lastEvt struct {
		ts   uint32
		done bool
	}
}

// Open allocates a socket pair.
func (p *Ports) Open(ctx context.Context) (*Endpoint, error) {
	a, b, err := p.alloc(ctx)
	if err != nil {
		return nil, err
	}
	e := &Endpoint{ports: p, rtp: a, rtcp: b, localPort: a.LocalAddr().(*net.UDPAddr).Port,
		rx: rtpstats.NewReceiver(clockRate), done: make(chan struct{})}
	var buf [18]byte
	_, _ = rand.Read(buf[:])
	e.ssrc = binary.BigEndian.Uint32(buf[0:4])
	e.seq = binary.BigEndian.Uint16(buf[4:6])
	e.ts = binary.BigEndian.Uint32(buf[6:10])
	e.cname = fmt.Sprintf("%x@opendectcell", buf[10:18]) // random, RFC 7022
	go e.readRTCP()
	go e.sendRTCP()
	return e, nil
}

// LocalPort is the RTP port to advertise in SDP.
func (e *Endpoint) LocalPort() int { return e.localPort }

// SSRC is the sending SSRC.
func (e *Endpoint) SSRC() uint32 { return e.ssrc }

// clockRate is the RTP clock of every codec the SIP side offers (G.711 and
// G.722 both count 8000 Hz, RFC 3551 §4.5.2), and of telephone-event.
const clockRate = 8000

// RTCPInterval is the mean reporting interval (RFC 3550 §6.2 minimum).
var RTCPInterval = 5 * time.Second

// readRTCP reads the peer's reports.
func (e *Endpoint) readRTCP() {
	buf := make([]byte, 1500)
	for {
		n, from, err := e.rtcp.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		e.mu.Lock()
		secure, recv, sdpRemote := e.secure, e.recvCtx, e.sdpRemote
		e.mu.Unlock()
		b := buf[:n]
		if secure {
			e.recvMu.Lock()
			b, err = recv.DecryptRTCP(nil, b, nil)
			e.recvMu.Unlock()
			if err != nil {
				continue
			}
		} else if !sdpRemote.IsValid() || from.Addr() != sdpRemote.Addr() {
			continue // plain RTCP only from the signalled peer
		}
		pkts, err := rtcp.Unmarshal(b)
		if err != nil {
			continue
		}
		now := time.Now()
		e.mu.Lock()
		e.rtcpRemote = from
		e.mu.Unlock()
		for _, p := range pkts {
			var reports []rtcp.ReceptionReport
			switch p := p.(type) {
			case *rtcp.SenderReport:
				e.rx.SenderReport(p.NTPTime, now)
				reports = p.Reports
			case *rtcp.ReceiverReport:
				reports = p.Reports
			}
			for _, r := range reports {
				if r.SSRC == e.ssrc {
					pr := rtpstats.FromReception(r, clockRate, now)
					e.mu.Lock()
					if pr.RTT == 0 {
						pr.RTT = e.peer.RTT // keep the last measurement
					}
					e.peer = pr
					e.mu.Unlock()
				}
			}
		}
	}
}

// sendRTCP sends a report at randomised intervals around RTCPInterval
// (0.5 to 1.5 times, RFC 3550 §6.3.1).
func (e *Endpoint) sendRTCP() {
	for {
		var b [1]byte
		_, _ = rand.Read(b[:])
		d := RTCPInterval/2 + RTCPInterval*time.Duration(b[0])/255
		select {
		case <-e.done:
			return
		case <-time.After(d):
		}
		_ = e.SendReport()
	}
}

// SendReport sends one compound RTCP packet now: a sender report if we have
// sent media, else a receiver report, with a reception report about the
// peer's stream and our CNAME.
func (e *Endpoint) SendReport() error {
	now := time.Now()
	var pkts []rtcp.Packet
	var reports []rtcp.ReceptionReport
	if rr, ok := e.rx.Report(now); ok {
		reports = append(reports, rr)
	}
	if sr, ok := e.tx.Report(e.ssrc, clockRate, now); ok {
		sr.Reports = reports
		pkts = append(pkts, &sr)
	} else {
		pkts = append(pkts, &rtcp.ReceiverReport{SSRC: e.ssrc, Reports: reports})
	}
	pkts = append(pkts, &rtcp.SourceDescription{Chunks: []rtcp.SourceDescriptionChunk{{
		Source: e.ssrc, Items: []rtcp.SourceDescriptionItem{{Type: rtcp.SDESCNAME, Text: e.cname}}}}})
	b, err := rtcp.Marshal(pkts)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return net.ErrClosed
	}
	to := e.rtcpRemote
	if !to.IsValid() {
		if !e.remote.IsValid() || e.remote.Port() == 0 {
			return nil
		}
		to = netip.AddrPortFrom(e.remote.Addr(), e.remote.Port()+1)
	}
	if e.secure {
		if b, err = e.sendCtx.EncryptRTCP(nil, b, nil); err != nil {
			return err
		}
	}
	_, err = e.rtcp.WriteToUDPAddrPort(b, to)
	return err
}

// Stats are a SIP leg's media statistics.
type Stats struct {
	Received rtpstats.Stats      // the peer's stream, as we receive it
	Peer     rtpstats.PeerReport // our stream, as the peer reported it; zero until its first report
}

// Stats returns the media statistics so far.
func (e *Endpoint) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Stats{Received: e.rx.Stats(), Peer: e.peer}
}

func profile(suite string) (srtp.ProtectionProfile, int, error) {
	switch suite {
	case "AES_CM_128_HMAC_SHA1_80":
		return srtp.ProtectionProfileAes128CmHmacSha1_80, 16, nil
	case "AES_CM_128_HMAC_SHA1_32":
		return srtp.ProtectionProfileAes128CmHmacSha1_32, 16, nil
	case "AEAD_AES_128_GCM":
		return srtp.ProtectionProfileAeadAes128Gcm, 16, nil
	}
	return 0, 0, fmt.Errorf("rtp: unsupported SRTP suite %s", suite)
}

func srtpContext(c *sdp.Crypto) (*srtp.Context, error) {
	p, keyLen, err := profile(c.Suite)
	if err != nil {
		return nil, err
	}
	if len(c.Key) != sdp.KeyLen(c.Suite) {
		return nil, errors.New("rtp: SRTP key has the wrong length")
	}
	return srtp.CreateContext(c.Key[:keyLen], c.Key[keyLen:], p)
}

// Configure sets the negotiated media: where to send, and the SRTP keys.
// It can be called again after a re-INVITE.
func (e *Endpoint) Configure(m *sdp.Media) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	// A latched destination survives a re-INVITE that signals the same
	// address again; a new address starts over.
	prev := e.sdpRemote
	e.sdpRemote = m.Remote
	if !e.latched || m.Remote != prev {
		e.remote, e.latched = m.Remote, false
	}
	if m.Remote != prev {
		e.rtcpRemote = netip.AddrPort{}
	}
	e.telEventPT = uint8(m.TelEventPT)
	if (m.LocalKey == nil) != (m.RemoteKey == nil) {
		return errors.New("rtp: SRTP needs both keys")
	}
	if m.LocalKey == nil {
		e.secure, e.sendCtx, e.recvCtx = false, nil, nil
		return nil
	}
	var err error
	if e.sendCtx, err = srtpContext(m.LocalKey); err != nil {
		return err
	}
	if e.recvCtx, err = srtpContext(m.RemoteKey); err != nil {
		return err
	}
	e.secure = true
	return nil
}

// Secure reports whether media is SRTP.
func (e *Endpoint) Secure() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.secure
}

// Remote is where packets currently go.
func (e *Endpoint) Remote() netip.AddrPort {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.remote
}

// Write sends one RTP packet with payload type pt, advancing the timestamp
// by samples afterwards.
func (e *Endpoint) Write(pt uint8, payload []byte, samples uint32, marker bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.writeLocked(pt, payload, e.ts, marker)
	e.ts += samples
	return err
}

// Skip advances the timestamp by samples without sending, for time in which
// the media engine has nothing to send (a lost packet it forwards untouched).
func (e *Endpoint) Skip(samples uint32) {
	e.mu.Lock()
	e.ts += samples
	e.mu.Unlock()
}

func (e *Endpoint) writeLocked(pt uint8, payload []byte, ts uint32, marker bool) error {
	if e.closed {
		return net.ErrClosed
	}
	if !e.remote.IsValid() || e.remote.Port() == 0 {
		return nil // no destination (yet, or the stream is rejected)
	}
	pkt := prtp.Packet{Header: prtp.Header{Version: 2, PayloadType: pt, SequenceNumber: e.seq, Timestamp: ts, SSRC: e.ssrc, Marker: marker}, Payload: payload}
	e.seq++
	e.tx.Sent(ts, len(payload), time.Now())
	b, err := pkt.Marshal()
	if err != nil {
		return err
	}
	if e.secure {
		if b, err = e.sendCtx.EncryptRTP(nil, b, nil); err != nil {
			return err
		}
	}
	_, err = e.rtp.WriteToUDPAddrPort(b, e.remote)
	return err
}

var dtmfEvents = "0123456789*#ABCD"

// SendDTMF sends an RFC 4733 event: the start packet with the marker bit,
// updates every 50 ms, and the end packet three times. It blocks for the
// duration. The audio timestamp is held during the event.
func (e *Endpoint) SendDTMF(ctx context.Context, digit rune, d time.Duration) error {
	evt := -1
	for i, c := range dtmfEvents {
		if c == digit {
			evt = i
		}
	}
	if evt < 0 {
		return fmt.Errorf("rtp: %q is not a DTMF digit", digit)
	}
	e.mu.Lock()
	pt, ts := e.telEventPT, e.ts
	e.mu.Unlock()
	if pt == 0 {
		return errors.New("rtp: the peer did not negotiate telephone-event")
	}
	body := func(dur uint32, end bool) []byte {
		b := []byte{byte(evt), 10, 0, 0} // volume -10 dBm0
		if end {
			b[1] |= 0x80
		}
		binary.BigEndian.PutUint16(b[2:], uint16(min(dur, 0xFFFF)))
		return b
	}
	total := uint32(d / time.Millisecond * 8)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for dur := uint32(0); ; dur += 400 {
		if dur > total {
			dur = total
		}
		end := dur == total
		reps := 1
		if end {
			reps = 3
		}
		e.mu.Lock()
		for range reps {
			if err := e.writeLocked(pt, body(dur, end), ts, dur == 0); err != nil {
				e.mu.Unlock()
				return err
			}
		}
		if end {
			e.ts = ts + total
			e.mu.Unlock()
			return nil
		}
		e.mu.Unlock()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Packet is a received RTP packet.
type Packet struct {
	PT        uint8
	Seq       uint16
	Timestamp uint32
	SSRC      uint32
	Marker    bool
	Payload   []byte
}

// Read returns the next media packet. Telephone events are consumed and
// reported through OnDTMF. Packets that fail SRTP authentication, or that
// come from an address the endpoint may not latch to, are dropped.
func (e *Endpoint) Read(deadline time.Time) (Packet, error) {
	buf := make([]byte, 1500)
	for {
		if err := e.rtp.SetReadDeadline(deadline); err != nil {
			return Packet{}, err
		}
		n, from, err := e.rtp.ReadFromUDPAddrPort(buf)
		if err != nil {
			return Packet{}, err
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		e.mu.Lock()
		secure, recv := e.secure, e.recvCtx
		e.mu.Unlock()
		b := buf[:n]
		if secure {
			e.recvMu.Lock()
			b, err = recv.DecryptRTP(nil, b, nil)
			e.recvMu.Unlock()
			if err != nil {
				continue
			}
		}
		var p prtp.Packet
		if err := p.Unmarshal(b); err != nil || p.Version != 2 {
			continue
		}
		if !e.accept(from, secure) {
			continue
		}
		e.rx.Update(p.SSRC, p.SequenceNumber, p.Timestamp, time.Now())
		e.mu.Lock()
		tel := e.telEventPT
		e.mu.Unlock()
		if tel != 0 && p.PayloadType == tel {
			e.dtmf(p)
			continue
		}
		return Packet{PT: p.PayloadType, Seq: p.SequenceNumber, Timestamp: p.Timestamp, SSRC: p.SSRC, Marker: p.Marker, Payload: p.Payload}, nil
	}
}

// accept implements symmetric-RTP latching: packets from the signalled
// address are always taken. A different source is taken, and becomes the
// destination, only if it can be trusted: any source for SRTP (the packet
// authenticated), and for plain RTP only another port on the signalled IP
// (a NAT rewriting the port).
func (e *Endpoint) accept(from netip.AddrPort, secure bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if from == e.remote {
		return true
	}
	if secure || (e.sdpRemote.IsValid() && from.Addr() == e.sdpRemote.Addr()) {
		e.remote, e.latched = from, true
		return true
	}
	return false
}

func (e *Endpoint) dtmf(p prtp.Packet) {
	if len(p.Payload) < 4 || e.OnDTMF == nil {
		return
	}
	evt, end := int(p.Payload[0]), p.Payload[1]&0x80 != 0
	dur := binary.BigEndian.Uint16(p.Payload[2:])
	e.mu.Lock()
	if p.Timestamp != e.lastEvt.ts {
		e.lastEvt.ts, e.lastEvt.done = p.Timestamp, false
	}
	report := end && !e.lastEvt.done
	if report {
		e.lastEvt.done = true
	}
	e.mu.Unlock()
	if report && evt < len(dtmfEvents) {
		e.OnDTMF(rune(dtmfEvents[evt]), time.Duration(dur)*time.Second/8000)
	}
}

// Close releases the sockets.
func (e *Endpoint) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	close(e.done)
	e.mu.Unlock()
	err := errors.Join(e.rtp.Close(), e.rtcp.Close())
	e.ports.release(e.localPort)
	return err
}
