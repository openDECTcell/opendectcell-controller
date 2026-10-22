// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build interop

// Interop against a real PBX (AGENTS.md §7). CI runs it against Asterisk
// configured by test/asterisk/:
//
//	OPENDECTCELL_INTEROP_SIP=127.0.0.1:5060 go test -tags interop -run Interop ./internal/sip/
package sip_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

func interopAccount(user, transport string) sip.Account {
	return sip.Account{ID: uuid.New(), Label: user, Username: user, Domain: "127.0.0.1", Password: "interop-" + user,
		Registrar: os.Getenv("OPENDECTCELL_INTEROP_SIP"), Transport: resolve.Transport(transport), Register: true, RegisterExpiry: 120,
		Keepalive: "none", Encryption: sdp.EncOff, Codecs: []string{"G722", "PCMA", "PCMU"}, DTMFMode: "rfc4733",
		CallerIDSource: "auto", Enabled: true}
}

func TestInteropAsterisk(t *testing.T) {
	if os.Getenv("OPENDECTCELL_INTEROP_SIP") == "" {
		t.Skip("OPENDECTCELL_INTEROP_SIP not set")
	}
	for _, tr := range []string{"udp", "tcp"} {
		t.Run(tr, func(t *testing.T) { interop(t, tr) })
	}
}

func interop(t *testing.T, transport string) {
	ep := &endpoint{calls: make(chan *sip.Call, 4)}
	ep.action = func(c *sip.Call) { c.Ringing(); time.Sleep(200 * time.Millisecond); c.Answer() }
	m := newManager(t, ep, nil)
	a, b := interopAccount("6001", transport), interopAccount("6002", transport)
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

	// 6001 calls 6002 through Asterisk.
	out, err := m.Dial(context.Background(), a.ID, "6002")
	if err != nil {
		t.Fatal(err)
	}
	var in *sip.Call
	select {
	case in = <-ep.calls:
	case <-time.After(10 * time.Second):
		t.Fatal("6002 never rang")
	}
	if in.AccountID != b.ID || in.Remote != "6001" {
		t.Errorf("incoming leg: account %v, from %q", in.AccountID, in.Remote)
	}
	nextEvent(t, out, sip.EvAnswered)
	nextEvent(t, in, sip.EvAnswered)
	t.Logf("codecs: out %s, in %s", out.Negotiated().Codec.Name, in.Negotiated().Codec.Name)

	// Audio both ways through Asterisk's media relay.
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
