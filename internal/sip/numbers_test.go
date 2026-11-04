// SPDX-License-Identifier: AGPL-3.0-or-later

package sip_test

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	esip "github.com/emiago/sipgo/sip"

	"github.com/openDECTcell/opendectcell-controller/internal/numberplan"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

// A line's number rules rewrite the number in the request line of an outgoing call.
func TestOutgoingCallUsesTheLinesNumberRules(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["330"] = "secret-330" })
	answered := make(chan *inCall, 1)
	p.set(func() { p.answer = func(c *inCall) { c.answerCall(t, sdp.EncOptional); answered <- c } })
	m := newManager(t, nil, nil)
	a := account(p, "330")
	a.Encryption = sdp.EncOptional
	a.Numbers = numberplan.Rules{CountryCode: "49", AreaCode: "30", DialAs: numberplan.E164, StripPrefix: "9"}
	m.SetAccounts([]sip.Account{a})
	waitFor(t, "registration", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegRegistered })

	for typed, want := range map[string]string{"0301234567": "+49301234567", "1234567": "+49301234567", "9017012345": "+4917012345", "112": "112"} {
		c, err := m.Dial(context.Background(), a.ID, typed)
		if err != nil {
			t.Fatal(err)
		}
		nextEvent(t, c, sip.EvAnswered)
		in := <-answered
		if in.req.Recipient.User != want {
			t.Errorf("dialled %q: Request-URI user %q, want %q", typed, in.req.Recipient.User, want)
		}
		c.Hangup()
		nextEvent(t, c, sip.EvEnded)
	}
}

// An incoming number is shown as the line's rules say, and kept as it arrived.
func TestIncomingNumberIsShownByTheLinesRules(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["340"] = "secret-340" })
	ep := &endpoint{calls: make(chan *sip.Call, 4)}
	ep.action = func(c *sip.Call) { c.Ringing(); c.Answer() }
	m := newManager(t, ep, nil)
	a := account(p, "340")
	a.Numbers = numberplan.Rules{CountryCode: "49", AreaCode: "30", ShowAs: numberplan.National}
	m.SetAccounts([]sip.Account{a})
	waitFor(t, "registration", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegRegistered })

	_, _, offer, _ := pbxOffer(t, p, sdp.EncOff, "PCMA", "G722")
	pai := esip.NewHeader("P-Asserted-Identity", `"Reception" <sip:+49301111@pbx.test>`)
	sess, err := p.call(t, context.Background(), "340", offer, pai)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.WaitAnswer(context.Background(), sipgo.AnswerOptions{}); err != nil {
		t.Fatal(err)
	}
	sess.Ack(context.Background())
	select {
	case c := <-ep.calls:
		if c.Remote != "+49301111" || c.RemoteShown != "0301111" {
			t.Errorf("remote %q, shown %q", c.Remote, c.RemoteShown)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no call")
	}
}
