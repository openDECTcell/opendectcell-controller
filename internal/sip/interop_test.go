// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build interop

// Interop against a real PBX (AGENTS.md §7). CI runs it against Asterisk
// (test/asterisk/) and FreeSWITCH (vanilla configuration):
//
//	OPENDECTCELL_INTEROP_SIP=127.0.0.1:5060 go test -tags interop -run Interop ./internal/sip/
//
// Optional: OPENDECTCELL_INTEROP_USERS ("user:password,user:password", the
// caller first), OPENDECTCELL_INTEROP_DOMAIN, OPENDECTCELL_INTEROP_BIND (local IP for
// SIP and RTP) and OPENDECTCELL_INTEROP_TRANSPORTS ("udp,tcp").
package sip_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func interopAccount(cred, transport string) sip.Account {
	user, pw, _ := strings.Cut(cred, ":")
	return sip.Account{ID: uuid.New(), Label: user, Username: user, Domain: env("OPENDECTCELL_INTEROP_DOMAIN", "127.0.0.1"), Password: pw,
		Registrar: os.Getenv("OPENDECTCELL_INTEROP_SIP"), Transport: resolve.Transport(transport), Register: true, RegisterExpiry: 120,
		Keepalive: "none", Encryption: sdp.EncOff, Codecs: []string{"G722", "PCMA", "PCMU"}, DTMFMode: "rfc4733",
		CallerIDSource: "auto", Enabled: true}
}

func TestInteropPBX(t *testing.T) {
	if os.Getenv("OPENDECTCELL_INTEROP_SIP") == "" {
		t.Skip("OPENDECTCELL_INTEROP_SIP not set")
	}
	for _, tr := range strings.Split(env("OPENDECTCELL_INTEROP_TRANSPORTS", "udp,tcp"), ",") {
		t.Run(tr, func(t *testing.T) { interop(t, tr) })
	}
}

func interopManager(t *testing.T, ep sip.Endpoint) *sip.Manager {
	bind := netip.MustParseAddr(env("OPENDECTCELL_INTEROP_BIND", "127.0.0.1"))
	conn, err := net.ListenPacket("udp", netip.AddrPortFrom(bind, 0).String())
	if err != nil {
		t.Fatal(err)
	}
	// A TCP listener too, as in production: a PBX that doesn't reuse the
	// registration's connection (FreeSWITCH) connects to our Contact.
	tcp, err := net.Listen("tcp", netip.AddrPortFrom(bind, 0).String())
	if err != nil {
		t.Fatal(err)
	}
	m, err := sip.New(sip.Config{AllowLocal: true, UDP: conn, TCP: tcp, RTP: &rtp.Ports{IP: bind, Min: 42000, Max: 42199},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, ep)
	if err != nil {
		t.Fatal(err)
	}
	m.Start(context.Background())
	t.Cleanup(func() { m.Close() })
	return m
}

func interop(t *testing.T, transport string) {
	ep := &endpoint{calls: make(chan *sip.Call, 4)}
	ep.action = func(c *sip.Call) { c.Ringing(); time.Sleep(200 * time.Millisecond); c.Answer() }
	m := interopManager(t, ep)
	creds := strings.Split(env("OPENDECTCELL_INTEROP_USERS", "6001:interop-6001,6002:interop-6002"), ",")
	a, b := interopAccount(creds[0], transport), interopAccount(creds[1], transport)
	m.SetAccounts([]sip.Account{a, b})
	for _, acc := range []sip.Account{a, b} {
		waitFor(t, acc.Username+" registered", func() bool {
			st, _ := m.Status(acc.ID)
			if st.State == sip.RegFailed {
				t.Fatalf("%s: %s", acc.Username, st.LastError)
			}
			return st.State == sip.RegRegistered
		})
	}

	// The first user calls the second through the PBX.
	out, err := m.Dial(context.Background(), a.ID, b.Username)
	if err != nil {
		t.Fatal(err)
	}
	var in *sip.Call
	select {
	case in = <-ep.calls:
	case <-time.After(10 * time.Second):
		t.Fatal(b.Username + " never rang")
	}
	if in.AccountID != b.ID || in.Remote != a.Username {
		t.Errorf("incoming leg: account %v, from %q", in.AccountID, in.Remote)
	}
	nextEvent(t, out, sip.EvAnswered)
	nextEvent(t, in, sip.EvAnswered)
	t.Logf("codecs: out %s, in %s", out.Negotiated().Codec.Name, in.Negotiated().Codec.Name)

	// Audio both ways through the PBX's media relay.
	exchangeAudio(t, out.Media(), in.Media(), out.Negotiated().Codec.PT)

	// DTMF from one leg reaches the other.
	go func() {
		for {
			if _, err := in.Media().Read(time.Now().Add(3 * time.Second)); err != nil {
				return
			}
		}
	}()
	if err := out.SendDTMF(context.Background(), '5'); err != nil {
		t.Fatal(err)
	}
	if e := nextEvent(t, in, sip.EvDTMF); e.Digit != '5' {
		t.Errorf("digit %q", e.Digit)
	}

	// Hold and resume.
	if err := out.Hold(context.Background(), true); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := out.Hold(context.Background(), false); err != nil {
		t.Fatalf("resume: %v", err)
	}

	out.Hangup()
	nextEvent(t, out, sip.EvEnded)
	nextEvent(t, in, sip.EvEnded)
}
