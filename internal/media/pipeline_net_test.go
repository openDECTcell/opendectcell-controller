// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/media/codec"
	"github.com/openDECTcell/opendectcell-controller/internal/media/quality"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

var lo = netip.MustParseAddr("127.0.0.1")

func udp(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(lo, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func port(c *net.UDPConn) netip.AddrPort {
	return netip.AddrPortFrom(lo, uint16(c.LocalAddr().(*net.UDPAddr).Port))
}

// sendPaced sends src in 20 ms packets in real time.
func sendPaced(t *testing.T, l Leg, pt uint8, c *codec.Codec, src []int16) {
	enc := c.NewEncoder()
	n := c.SampleRate / 50
	tk := time.NewTicker(20 * time.Millisecond)
	defer tk.Stop()
	for i := 0; i+n <= len(src); i += n {
		<-tk.C
		if err := l.WritePacket(pt, enc.Encode(nil, src[i:i+n]), c.TimestampUnits(n), i == 0); err != nil {
			t.Error(err)
			return
		}
	}
}

// receive decodes what arrives on l until deadline, placed by timestamp.
func receive(l Leg, c *codec.Codec, until time.Time) []int16 {
	dec := c.NewDecoder()
	var out []int16
	var ts0 uint32
	first := true
	for {
		p, err := l.ReadPacket(until)
		if err != nil {
			return out
		}
		if first {
			ts0, first = p.TS, false
		}
		at := int(p.TS-ts0) * c.SampleRate / c.ClockRate
		for len(out) < at {
			out = append(out, 0)
		}
		if len(out) > at {
			out = out[:at] // a late or duplicate packet: overwrite
		}
		out = dec.Decode(out, p.Payload)
	}
}

// A known signal crosses the whole engine in both directions over real
// sockets: a SIP peer with SDES-SRTP, and a cell with SRTP keyed by
// MEDIA_OPEN, L16 on the cell link and PCMA on SIP.
func TestEndToEndOverUDP(t *testing.T) {
	// SIP: the controller's endpoint and the PBX's.
	ports := &rtp.Ports{IP: lo, Min: 42000, Max: 42100}
	ctl, err := ports.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pbx, err := ports.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	defer pbx.Close()
	const suite = "AES_CM_128_HMAC_SHA1_80"
	k1, _ := sdp.NewKey(suite)
	k2, _ := sdp.NewKey(suite)
	if err := ctl.Configure(&sdp.Media{Remote: netip.AddrPortFrom(lo, uint16(pbx.LocalPort())), Codec: sdp.Codec{Name: "PCMA", PT: 8, Clock: 8000},
		LocalKey: &sdp.Crypto{Suite: suite, Key: k1}, RemoteKey: &sdp.Crypto{Suite: suite, Key: k2}}); err != nil {
		t.Fatal(err)
	}
	if err := pbx.Configure(&sdp.Media{Remote: netip.AddrPortFrom(lo, uint16(ctl.LocalPort())), Codec: sdp.Codec{Name: "PCMA", PT: 8, Clock: 8000},
		LocalKey: &sdp.Crypto{Suite: suite, Key: k2}, RemoteKey: &sdp.Crypto{Suite: suite, Key: k1}}); err != nil {
		t.Fatal(err)
	}

	// Cell: MEDIA_OPEN as the controller would send it, and the cell's
	// mirror of it.
	plan, err := Choose(narrowCell, false, codec.PCMA)
	if err != nil {
		t.Fatal(err)
	}
	ctlConn, cellConn := udp(t), udp(t)
	mo, err := NewMediaOpen(1, 1, plan, 96, port(ctlConn), wire.SRTPAES128SHA80)
	if err != nil {
		t.Fatal(err)
	}
	cellLeg, err := NewCellLeg(ctlConn, mo, 8000)
	if err != nil {
		t.Fatal(err)
	}
	defer cellLeg.Close()
	mirror := mo
	mirror.SSRCUp, mirror.SSRCDown, mirror.KeyUp, mirror.KeyDown = mo.SSRCDown, mo.SSRCUp, mo.KeyDown, mo.KeyUp
	cell, err := NewCellLeg(cellConn, mirror, 8000)
	if err != nil {
		t.Fatal(err)
	}
	defer cell.Close()
	cell.remote = mo.CtlAddr // the cell knows ctl_addr from MEDIA_OPEN

	p, err := New(Config{Plan: plan, SIP: SIPLeg(ctl), SIPPayloadType: 8, Cell: cellLeg, CellPayload: 96})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error)
	go func() { runDone <- p.Run(ctx) }()

	down := quality.SpeechLike(-20, 8000, 8000*2, 21)
	up := quality.SpeechLike(-20, 8000, 8000*2, 22)
	until := time.Now().Add(3 * time.Second)
	var gotDown, gotUp []int16
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); sendPaced(t, cell, 96, codec.L16, up) }()
	go func() { defer wg.Done(); sendPaced(t, SIPLeg(pbx), 8, codec.PCMA, down) }()
	go func() { defer wg.Done(); gotDown = receive(cell, codec.L16, until) }()
	go func() { defer wg.Done(); gotUp = receive(SIPLeg(pbx), codec.PCMA, until) }()
	wg.Wait()
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}

	// Real time on a shared machine: judge by envelope, not waveform.
	for _, c := range []struct {
		name     string
		ref, got []int16
	}{{"PBX → cell", down, gotDown}, {"cell → PBX", up, gotUp}} {
		env := quality.EnvelopeCorrelation(c.ref, c.got, 8000, 25)
		t.Logf("%s: %d samples, envelope r = %.3f", c.name, len(c.got), env)
		if env < 0.9 {
			t.Errorf("%s: envelope correlation %.3f; the path does not carry the source", c.name, env)
		}
	}
	st := p.Stats(time.Now())
	if st.Down.Sent < 100 || st.Up.Sent < 100 {
		t.Errorf("sent %d down, %d up in 3 s", st.Down.Sent, st.Up.Sent)
	}
	if cs := cellLeg.Stats(); cs.Received < 90 || cs.Lost != 0 {
		t.Errorf("cell leg reception %+v", cs)
	}
	if !cellLeg.Remote().IsValid() {
		t.Error("the cell leg never latched")
	}
}

// A cell leg sends nothing until the cell has sent, and ignores packets
// that do not authenticate.
func TestCellLegLatching(t *testing.T) {
	ctlConn, cellConn, other := udp(t), udp(t), udp(t)
	defer other.Close()
	mo, _ := NewMediaOpen(1, 1, Plan{CellDown: 1, CellUp: 1}, 96, port(ctlConn), wire.SRTPAES128SHA80)
	l, err := NewCellLeg(ctlConn, mo, 8000)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.WritePacket(96, make([]byte, 320), 160, true); err != nil {
		t.Fatal(err)
	}
	// A plain RTP packet with the right SSRC from elsewhere is not enough.
	forged := []byte{0x80, 96, 0, 1, 0, 0, 0, 0, byte(mo.SSRCUp >> 24), byte(mo.SSRCUp >> 16), byte(mo.SSRCUp >> 8), byte(mo.SSRCUp), 1, 2}
	if _, err := other.WriteToUDPAddrPort(forged, mo.CtlAddr); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReadPacket(time.Now().Add(200 * time.Millisecond)); err == nil {
		t.Fatal("a forged packet was accepted")
	}
	if l.Remote().IsValid() {
		t.Fatal("latched to a forged packet")
	}
	mirror := mo
	mirror.SSRCUp, mirror.SSRCDown, mirror.KeyUp, mirror.KeyDown = mo.SSRCDown, mo.SSRCUp, mo.KeyDown, mo.KeyUp
	cell, _ := NewCellLeg(cellConn, mirror, 8000)
	defer cell.Close()
	cell.remote = mo.CtlAddr
	if err := cell.WritePacket(96, make([]byte, 320), 160, true); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReadPacket(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if l.Remote() != port(cellConn) {
		t.Errorf("latched to %v, want %v", l.Remote(), port(cellConn))
	}
}
