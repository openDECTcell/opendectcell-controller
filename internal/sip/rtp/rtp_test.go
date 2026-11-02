// SPDX-License-Identifier: AGPL-3.0-or-later

package rtp

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

var ctx = context.Background()

func pair(t *testing.T, suite string) (*Endpoint, *Endpoint) {
	t.Helper()
	p := &Ports{IP: netip.MustParseAddr("127.0.0.1"), Min: 41000, Max: 41100}
	a, err := p.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	if a.LocalPort()%2 != 0 || a.LocalPort() == b.LocalPort() {
		t.Fatalf("ports %d %d", a.LocalPort(), b.LocalPort())
	}
	ma := &sdp.Media{Remote: netip.AddrPortFrom(p.IP, uint16(b.LocalPort())), TelEventPT: 101}
	mb := &sdp.Media{Remote: netip.AddrPortFrom(p.IP, uint16(a.LocalPort())), TelEventPT: 101}
	if suite != "" {
		ka, _ := sdp.NewKey(suite)
		kb, _ := sdp.NewKey(suite)
		ma.LocalKey, ma.RemoteKey = &sdp.Crypto{Suite: suite, Key: ka}, &sdp.Crypto{Suite: suite, Key: kb}
		mb.LocalKey, mb.RemoteKey = &sdp.Crypto{Suite: suite, Key: kb}, &sdp.Crypto{Suite: suite, Key: ka}
	}
	if err := a.Configure(ma); err != nil {
		t.Fatal(err)
	}
	if err := b.Configure(mb); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestMediaPlainAndSRTP(t *testing.T) {
	for _, suite := range []string{"", "AES_CM_128_HMAC_SHA1_80", "AES_CM_128_HMAC_SHA1_32", "AEAD_AES_128_GCM"} {
		a, b := pair(t, suite)
		payload := make([]byte, 160)
		for i := range payload {
			payload[i] = byte(i)
		}
		for i := range 3 {
			if err := a.Write(9, payload, 160, i == 0); err != nil {
				t.Fatalf("%q write: %v", suite, err)
			}
		}
		var first Packet
		for i := range 3 {
			p, err := b.Read(time.Now().Add(time.Second))
			if err != nil {
				t.Fatalf("%q read %d: %v", suite, i, err)
			}
			if i == 0 {
				first = p
				if !p.Marker {
					t.Errorf("%q: marker lost", suite)
				}
			}
			if p.PT != 9 || len(p.Payload) != 160 || p.Payload[100] != 100 || p.SSRC != a.SSRC() {
				t.Fatalf("%q packet %+v", suite, p)
			}
			if p.Timestamp != first.Timestamp+uint32(160*i) || p.Seq != first.Seq+uint16(i) {
				t.Errorf("%q: ts/seq %d/%d", suite, p.Timestamp, p.Seq)
			}
		}
		if (suite != "") != b.Secure() {
			t.Errorf("%q: Secure() = %v", suite, b.Secure())
		}
	}
}

func TestSRTPWrongKeyDropped(t *testing.T) {
	a, b := pair(t, "AES_CM_128_HMAC_SHA1_80")
	k, _ := sdp.NewKey("AES_CM_128_HMAC_SHA1_80")
	wrong := &sdp.Media{Remote: a.Remote(), LocalKey: &sdp.Crypto{Suite: "AES_CM_128_HMAC_SHA1_80", Key: k}, RemoteKey: &sdp.Crypto{Suite: "AES_CM_128_HMAC_SHA1_80", Key: k}}
	a.Configure(wrong)
	a.Write(0, make([]byte, 160), 160, false)
	if _, err := b.Read(time.Now().Add(200 * time.Millisecond)); err == nil {
		t.Fatal("packet with the wrong key accepted")
	}
}

func TestLatching(t *testing.T) {
	_, b := pair(t, "")
	// A plain packet from another IP is ignored, and does not move b's target.
	stranger, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.2")})
	if err != nil {
		t.Skip("127.0.0.2 not available:", err)
	}
	defer stranger.Close()
	pkt := []byte{0x80, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 9, 1, 2}
	stranger.WriteToUDPAddrPort(pkt, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(b.LocalPort())))
	if _, err := b.Read(time.Now().Add(200 * time.Millisecond)); err == nil {
		t.Fatal("plain packet from a foreign IP accepted")
	}
	// From the signalled IP but another port (NAT): accepted, and latched.
	nat, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	defer nat.Close()
	nat.WriteToUDPAddrPort(pkt, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(b.LocalPort())))
	if _, err := b.Read(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("NAT-rewritten port: %v", err)
	}
	if got := b.Remote().Port(); got != uint16(nat.LocalAddr().(*net.UDPAddr).Port) {
		t.Fatalf("not latched: %v", b.Remote())
	}
}

func TestDTMF(t *testing.T) {
	a, b := pair(t, "AES_CM_128_HMAC_SHA1_80")
	var mu sync.Mutex
	var got []rune
	var dur time.Duration
	b.OnDTMF = func(d rune, l time.Duration) { mu.Lock(); got = append(got, d); dur = l; mu.Unlock() }
	done := make(chan struct{})
	go func() {
		for {
			if _, err := b.Read(time.Now().Add(100 * time.Millisecond)); err != nil {
				select {
				case <-done:
					return
				default:
				}
			}
		}
	}()
	for _, d := range "5#" {
		if err := a.SendDTMF(ctx, d, 120*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(150 * time.Millisecond)
	close(done)
	mu.Lock()
	defer mu.Unlock()
	if string(got) != "5#" {
		t.Fatalf("digits %q (each must be reported once)", string(got))
	}
	if dur != 120*time.Millisecond {
		t.Errorf("duration %v", dur)
	}
	if err := a.SendDTMF(ctx, 'x', time.Millisecond); err == nil {
		t.Error("invalid digit sent")
	}
}

func TestPortsExhaust(t *testing.T) {
	p := &Ports{IP: netip.MustParseAddr("127.0.0.1"), Min: 41200, Max: 41203}
	a, err := p.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(ctx); !errors.Is(err, ErrNoPorts) {
		t.Fatalf("third pair: %v", err)
	}
	a.Close()
	c, err := p.Open(ctx)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	b.Close()
	c.Close()
}

// Both ends exchange reports: the receiver's report about our stream comes
// back with the loss we caused and a round-trip time, plain and with SRTCP.
func TestRTCP(t *testing.T) {
	for _, suite := range []string{"", "AES_CM_128_HMAC_SHA1_80"} {
		a, b := pair(t, suite)
		for i := range 50 {
			if i == 20 || i == 30 {
				a.mu.Lock()
				a.seq++ // a packet that never leaves: the peer sees a gap
				a.mu.Unlock()
				continue
			}
			if err := a.Write(8, make([]byte, 160), 160, false); err != nil {
				t.Fatal(err)
			}
		}
		for range 48 {
			if _, err := b.Read(time.Now().Add(time.Second)); err != nil {
				t.Fatalf("%q read: %v", suite, err)
			}
		}
		if s := b.Stats().Received; s.Lost != 2 || s.Received != 47 {
			t.Errorf("%q: receiver stats %+v", suite, s)
		}
		// a sends an SR; once b has it, b's RR carries LSR/DLSR back.
		if err := a.SendReport(); err != nil {
			t.Fatal(err)
		}
		// b reports until one of its reports carries the SR's timestamp.
		waitFor(t, func() bool {
			if err := b.SendReport(); err != nil {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
			return a.Stats().Peer.RTT > 0
		})
		p := a.Stats().Peer
		if p.Lost != 2 || p.RTT <= 0 || p.RTT > time.Second {
			t.Errorf("%q: peer report %+v", suite, p)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}
