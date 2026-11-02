// SPDX-License-Identifier: AGPL-3.0-or-later

package sip_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
)

func TestParseMessageSummary(t *testing.T) {
	for _, c := range []struct {
		body string
		want sip.MWI
	}{
		{"Messages-Waiting: yes\r\nMessage-Account: sip:301@pbx.test\r\nVoice-Message: 2/8 (0/1)\r\n", sip.MWI{Waiting: true, New: 2}},
		{"Messages-Waiting: no\r\nVoice-Message: 0/3 (0/0)\r\n", sip.MWI{}},
		{"Messages-Waiting: yes\r\n", sip.MWI{Waiting: true, New: 1}},
		{"messages-waiting: NO\n", sip.MWI{}},
		{"Voice-Message: 4/0\n", sip.MWI{Waiting: true, New: 4}},
		{"", sip.MWI{}},
		{"nonsense", sip.MWI{}},
	} {
		if got := sip.ParseMessageSummary([]byte(c.body)); got != c.want {
			t.Errorf("%q: %+v, want %+v", c.body, got, c.want)
		}
	}
}

// An account with MWI on subscribes to its mailbox after it registered (answering the
// registrar's digest challenge), and what a NOTIFY says reaches OnMWI; a NOTIFY for an
// account without the switch is answered but not reported.
func TestMessageWaiting(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["301"] = "secret-301"; p.users["302"] = "secret-302" })
	got := make(chan sip.MWI, 8)
	ids := make(chan uuid.UUID, 8)
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m, err := sip.New(sip.Config{UDP: conn, RTP: &rtp.Ports{IP: netip.MustParseAddr("127.0.0.1"), Min: 42200, Max: 42299},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), AllowLocal: true,
		OnMWI: func(id uuid.UUID, s sip.MWI) { ids <- id; got <- s }}, &endpoint{calls: make(chan *sip.Call, 1)})
	if err != nil {
		t.Fatal(err)
	}
	m.Start(context.Background())
	t.Cleanup(func() { m.Close() })

	a, off := account(p, "301"), account(p, "302")
	a.MWI = true
	m.SetAccounts([]sip.Account{a, off})
	waitFor(t, "registration", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegRegistered })
	waitFor(t, "registration 2", func() bool { st, _ := m.Status(off.ID); return st.State == sip.RegRegistered })

	select {
	case req := <-p.subs:
		if ev := req.GetHeader("Event"); ev == nil || ev.Value() != "message-summary" {
			t.Fatalf("Event %v", ev)
		}
		p.notifyMWI(t, "301", req, "Messages-Waiting: yes\r\nVoice-Message: 3/0 (0/0)\r\n")
		select {
		case s := <-got:
			if !s.Waiting || s.New != 3 || <-ids != a.ID {
				t.Errorf("reported %+v", s)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the NOTIFY did not reach OnMWI")
		}
		p.notifyMWI(t, "301", req, "Messages-Waiting: no\r\n")
		if s := <-got; s.Waiting {
			t.Errorf("after the messages were heard: %+v", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no SUBSCRIBE from the account that has MWI on")
	}
	select {
	case <-p.subs:
		t.Error("the account without MWI subscribed")
	case <-time.After(300 * time.Millisecond):
	}
}
