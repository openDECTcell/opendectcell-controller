// SPDX-License-Identifier: AGPL-3.0-or-later

package sip_test

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	esip "github.com/emiago/sipgo/sip"
	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

// endpoint stands in for the DECT side.
type endpoint struct {
	mu     sync.Mutex
	calls  chan *sip.Call
	action func(*sip.Call)
}

func (e *endpoint) IncomingCall(c *sip.Call) {
	e.calls <- c
	e.mu.Lock()
	a := e.action
	e.mu.Unlock()
	if a != nil {
		go a(c)
	}
}

func newManager(t *testing.T, ep sip.Endpoint, dns resolve.DNS) *sip.Manager {
	t.Helper()
	return newManagerRoots(t, ep, dns, nil)
}

func newManagerRoots(t *testing.T, ep sip.Endpoint, dns resolve.DNS, roots *x509.CertPool) *sip.Manager {
	t.Helper()
	return newManagerHook(t, ep, dns, roots, nil)
}

func newManagerHook(t *testing.T, ep sip.Endpoint, dns resolve.DNS, roots *x509.CertPool, hook func(uuid.UUID, sip.RegStatus, time.Duration)) *sip.Manager {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m, err := sip.New(sip.Config{UDP: conn, RTP: &rtp.Ports{IP: netip.MustParseAddr("127.0.0.1"), Min: 42000, Max: 42199},
		DNS: dns, TLSRoots: roots, AllowLocal: true, OnRegChange: hook, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, ep)
	if err != nil {
		t.Fatal(err)
	}
	m.Start(context.Background())
	t.Cleanup(func() { m.Close() })
	return m
}

func account(p *pbx, user string) sip.Account {
	return sip.Account{ID: uuid.New(), Label: user, DisplayName: "Paula", Username: user, Domain: "pbx.test", Password: "secret-" + user,
		Registrar: p.addr.String(), Transport: resolve.UDP, Register: true, RegisterExpiry: 600, Keepalive: "auto",
		Encryption: sdp.EncOff, Codecs: []string{"G722", "PCMA", "PCMU"}, DTMFMode: "rfc4733", CallerIDSource: "auto", Enabled: true}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second) // the full gate runs the packages in parallel under -race: 5 s was missed once
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func nextEvent(t *testing.T, c *sip.Call, want sip.EventKind) sip.Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-c.Events():
			if !ok {
				t.Fatalf("events closed while waiting for %s", want)
			}
			if e.Kind == want {
				return e
			}
			if e.Kind == sip.EvEnded {
				t.Fatalf("call ended (%d %s) while waiting for %s", e.Code, e.Reason, want)
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

func TestRegistration(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.algos = []string{"MD5", "SHA-256"} }) // RFC 8760: we must pick SHA-256
	p.set(func() { p.users["301"] = "secret-301" })
	p.set(func() { p.expires = 4 }) // short, to see a refresh
	m := newManager(t, nil, nil)
	a := account(p, "301")
	m.SetAccounts([]sip.Account{a})
	waitFor(t, "registration", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegRegistered })
	contact, n := p.registered("301")
	if contact.User != sip.ContactUser(a.ID) || contact.Host != "127.0.0.1" || n != 1 {
		t.Fatalf("binding %v (%d registers)", contact, n)
	}
	p.mu.Lock()
	sha := p.registers["301/SHA-256"]
	via := p.seenVia[0]
	p.mu.Unlock()
	if sha != 1 {
		t.Errorf("did not answer the SHA-256 challenge")
	}
	if strings.Contains(via, "0.0.0.0") || !strings.Contains(via, "rport") {
		t.Errorf("Via %q", via)
	}
	st, _ := m.Status(a.ID)
	if st.Server != "udp:"+p.addr.String() || time.Until(st.RegisteredUntil) > 5*time.Second {
		t.Errorf("status %+v", st)
	}
	// Refreshed before the 4 s binding runs out.
	waitFor(t, "refresh", func() bool { _, n := p.registered("301"); return n >= 2 })

	// Removing the account unregisters it.
	m.SetAccounts(nil)
	waitFor(t, "unregister", func() bool { c, _ := p.registered("301"); return c.Host == "" })
	if _, ok := m.Status(a.ID); ok {
		t.Error("removed account still has a status")
	}
}

func TestRegistrationFailures(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["302"] = "the-right-one" })
	m := newManager(t, nil, nil)
	a := account(p, "302") // wrong password
	disabled := account(p, "303")
	disabled.Register = false
	m.SetAccounts([]sip.Account{a, disabled})
	waitFor(t, "failed state", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegFailed })
	if st, _ := m.Status(a.ID); !strings.HasPrefix(st.LastError, "401") || strings.Contains(st.LastError, "secret") {
		t.Errorf("last error %q", st.LastError)
	}
	if st, _ := m.Status(disabled.ID); st.State != sip.RegDisabled {
		t.Errorf("registration off: %s", st.State)
	}
	// Invalid accounts are skipped, not started.
	bad := account(p, "304")
	bad.Codecs = []string{"GSM"}
	m.SetAccounts([]sip.Account{bad})
	if _, ok := m.Status(bad.ID); ok {
		t.Error("invalid account started")
	}
}

type fakeDNS struct{ srv map[string][]*net.SRV }

func (f fakeDNS) LookupNAPTR(context.Context, string) ([]resolve.NAPTR, error) {
	return nil, errors.New("NXDOMAIN")
}
func (f fakeDNS) LookupSRV(_ context.Context, svc, proto, name string) ([]*net.SRV, error) {
	if r, ok := f.srv["_"+svc+"._"+proto+"."+name]; ok {
		return r, nil
	}
	return nil, errors.New("NXDOMAIN")
}
func (f fakeDNS) LookupNetIP(_ context.Context, host string) ([]netip.Addr, error) {
	if host == "sip.pbx.test" {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	return nil, errors.New("NXDOMAIN")
}

func TestFailoverToNextSRVTarget(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["305"] = "secret-305" })
	dead, _ := net.ListenPacket("udp", "127.0.0.1:0") // answers nothing
	defer dead.Close()
	dns := fakeDNS{srv: map[string][]*net.SRV{"_sip._udp.pbx.test": {
		{Target: "sip.pbx.test.", Port: uint16(dead.LocalAddr().(*net.UDPAddr).Port), Priority: 10},
		{Target: "sip.pbx.test.", Port: p.addr.Port(), Priority: 20},
	}}}
	m := newManager(t, nil, dns)
	a := account(p, "305")
	a.Registrar = "" // resolve the domain
	m.SetAccounts([]sip.Account{a})
	waitFor(t, "registration via the second target", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegRegistered })
	if st, _ := m.Status(a.ID); st.Server != "udp:"+p.addr.String() {
		t.Errorf("registered at %s", st.Server)
	}
}

// pbxMedia opens the PBX side's RTP for a call it places.
func pbxOffer(t *testing.T, p *pbx, enc sdp.Encryption, codecs ...string) (*rtp.Endpoint, sdp.Local, []byte, []sdp.Crypto) {
	ep, err := p.rtp.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ep.Close() })
	l := sdp.Local{Addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(ep.LocalPort())), Codecs: codecs, Encryption: enc}
	body, keys, err := sdp.Offer(l)
	if err != nil {
		t.Fatal(err)
	}
	return ep, l, body, keys
}

func exchangeAudio(t *testing.T, a, b *rtp.Endpoint, pt uint8) {
	t.Helper()
	for _, dir := range [][2]*rtp.Endpoint{{a, b}, {b, a}} {
		got := make(chan rtp.Packet, 1)
		go func(r *rtp.Endpoint) {
			p, err := r.Read(time.Now().Add(2 * time.Second))
			if err == nil {
				got <- p
			}
			close(got)
		}(dir[1])
		// A few packets, in case the first is sent before the reader runs.
		for range 5 {
			dir[0].Write(pt, make([]byte, 160), 160, false)
			time.Sleep(10 * time.Millisecond)
		}
		p, ok := <-got
		if !ok || p.PT != pt {
			t.Fatalf("no audio %d→%d (%+v)", dir[0].LocalPort(), dir[1].LocalPort(), p)
		}
	}
}

func TestOutgoingCall(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["310"] = "secret-310" })
	p.set(func() { p.authInvite = true })
	answered := make(chan *inCall, 1)
	p.set(func() { p.answer = func(c *inCall) { c.answerCall(t, sdp.EncOptional); answered <- c } })
	m := newManager(t, nil, nil)
	a := account(p, "310")
	a.Encryption = sdp.EncOptional
	m.SetAccounts([]sip.Account{a})
	waitFor(t, "registration", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegRegistered })

	c, err := m.Dial(context.Background(), a.ID, "+491701234567")
	if err != nil {
		t.Fatal(err)
	}
	nextEvent(t, c, sip.EvRinging)
	nextEvent(t, c, sip.EvAnswered)
	in := <-answered
	if in.req.Recipient.User != "+491701234567" || in.req.Recipient.Host != "pbx.test" {
		t.Errorf("Request-URI %v", in.req.Recipient)
	}
	if in.req.GetHeader("Proxy-Authorization") == nil {
		t.Error("INVITE was not re-sent with credentials")
	}
	neg := c.Negotiated()
	if neg.Codec.Name != "G722" || neg.LocalKey == nil {
		t.Fatalf("negotiated %+v", neg)
	}
	exchangeAudio(t, c.Media(), in.media, 9)

	// DTMF (RFC 4733) reaches the PBX's media endpoint.
	got := make(chan rune, 4)
	in.media.OnDTMF = func(d rune, _ time.Duration) { got <- d }
	go func() {
		for {
			if _, err := in.media.Read(time.Now().Add(3 * time.Second)); err != nil {
				return
			}
		}
	}()
	if err := c.SendDTMF(context.Background(), '7'); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-got:
		if d != '7' {
			t.Errorf("digit %q", d)
		}
	case <-time.After(2 * time.Second):
		t.Error("DTMF not received")
	}

	// Hold: the PBX sees a re-INVITE with sendonly.
	if err := c.Hold(context.Background(), true); err != nil {
		t.Fatalf("hold: %v", err)
	}
	re := <-p.reinvites
	if !strings.Contains(string(re.Body()), "a=sendonly") {
		t.Errorf("hold offer:\n%s", re.Body())
	}
	if err := c.Hold(context.Background(), false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if re := <-p.reinvites; !strings.Contains(string(re.Body()), "a=sendrecv") {
		t.Errorf("resume offer:\n%s", re.Body())
	}

	c.Hangup()
	select {
	case id := <-p.byes:
		if id != in.req.CallID().Value() {
			t.Errorf("BYE for %s", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no BYE")
	}
	if e := nextEvent(t, c, sip.EvEnded); e.Reason != "local hangup" {
		t.Errorf("end %+v", e)
	}
}

func TestOutgoingRejectedAndCancelled(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["311"] = "secret-311" })
	hold := make(chan *inCall, 1)
	p.set(func() {
		p.answer = func(c *inCall) {
			if c.req.Recipient.User == "busy" {
				c.sess.Respond(486, "Busy Here", nil)
				return
			}
			c.sess.Respond(180, "Ringing", nil)
			hold <- c
		}
	})
	m := newManager(t, nil, nil)
	a := account(p, "311")
	m.SetAccounts([]sip.Account{a})
	waitFor(t, "registration", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegRegistered })

	c, _ := m.Dial(context.Background(), a.ID, "busy")
	if e := nextEvent(t, c, sip.EvEnded); e.Code != 486 {
		t.Errorf("busy: %+v", e)
	}
	c, _ = m.Dial(context.Background(), a.ID, "ringforever")
	nextEvent(t, c, sip.EvRinging)
	in := <-hold
	c.Hangup() // before an answer: CANCEL
	select {
	case <-in.sess.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("PBX side not cancelled")
	}
	if e := nextEvent(t, c, sip.EvEnded); e.Code != 487 {
		t.Errorf("cancel: %+v", e)
	}
	if _, err := m.Dial(context.Background(), uuid.New(), "1"); !errors.Is(err, sip.ErrNoAccount) {
		t.Errorf("unknown account: %v", err)
	}
	if _, err := m.Dial(context.Background(), a.ID, "1 2"); err == nil {
		t.Error("number with a space accepted")
	}
}

func TestIncomingCall(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["320"] = "secret-320" })
	ep := &endpoint{calls: make(chan *sip.Call, 4)}
	ep.action = func(c *sip.Call) {
		c.Ringing()
		time.Sleep(50 * time.Millisecond)
		c.Answer()
	}
	m := newManager(t, ep, nil)
	a := account(p, "320")
	m.SetAccounts([]sip.Account{a})
	waitFor(t, "registration", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegRegistered })

	media, local, offer, keys := pbxOffer(t, p, sdp.EncOff, "PCMA", "G722")
	pai := esip.NewHeader("P-Asserted-Identity", `"Reception" <sip:+49301111@pbx.test>`)
	sess, err := p.call(t, context.Background(), "320", offer, pai)
	if err != nil {
		t.Fatal(err)
	}
	var ringing bool
	if err := sess.WaitAnswer(context.Background(), sipgo.AnswerOptions{OnResponse: func(r *esip.Response) error {
		ringing = ringing || r.StatusCode == 180
		return nil
	}}); err != nil {
		t.Fatalf("PBX's call not answered: %v", err)
	}
	sess.Ack(context.Background())
	c := <-ep.calls
	if !ringing {
		t.Error("no 180 before the answer")
	}
	if c.Remote != "+49301111" || c.RemoteDisplay != "Reception" || c.Direction != sip.Incoming || c.AccountID != a.ID {
		t.Errorf("call %+v", c)
	}
	nextEvent(t, c, sip.EvAnswered)
	neg, err := sdp.ParseAnswer(sess.InviteResponse.Body(), local, keys)
	if err != nil || neg.Codec.Name != "G722" {
		t.Fatalf("answer: %+v %v", neg, err)
	}
	media.Configure(neg)
	exchangeAudio(t, media, c.Media(), 9)

	// The PBX puts us on hold.
	hl := local
	hl.Direction = sdp.SendOnly
	hl.Version = 2
	holdOffer, _, _ := sdp.Offer(hl)
	re := esip.NewRequest(esip.INVITE, sess.InviteResponse.Contact().Address)
	re.SetBody(holdOffer)
	re.AppendHeader(esip.NewHeader("Content-Type", "application/sdp"))
	res, err := sess.Do(context.Background(), re)
	if err != nil || res.StatusCode != 200 || !strings.Contains(string(res.Body()), "a=recvonly") {
		t.Fatalf("hold re-INVITE: %v %v\n%s", res, err, res.Body())
	}
	nextEvent(t, c, sip.EvHeld)

	// DTMF via INFO.
	info := esip.NewRequest(esip.INFO, sess.InviteResponse.Contact().Address)
	info.SetBody([]byte("Signal=#\r\nDuration=160\r\n"))
	info.AppendHeader(esip.NewHeader("Content-Type", "application/dtmf-relay"))
	if res, err := sess.Do(context.Background(), info); err != nil || res.StatusCode != 200 {
		t.Fatalf("INFO: %v %v", res, err)
	}
	if e := nextEvent(t, c, sip.EvDTMF); e.Digit != '#' {
		t.Errorf("digit %q", e.Digit)
	}

	// The PBX hangs up.
	if err := sess.Bye(context.Background()); err != nil {
		t.Fatalf("BYE: %v", err)
	}
	nextEvent(t, c, sip.EvEnded)
}

func TestIncomingCancelRejectAndUnknown(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["330"] = "secret-330" })
	ep := &endpoint{calls: make(chan *sip.Call, 4)}
	ep.action = func(c *sip.Call) { c.Ringing() }
	m := newManager(t, ep, nil)
	a := account(p, "330")
	m.SetAccounts([]sip.Account{a})
	waitFor(t, "registration", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegRegistered })

	// The caller gives up while it rings.
	_, _, offer, _ := pbxOffer(t, p, sdp.EncOff, "PCMA")
	ctx, cancel := context.WithCancel(context.Background())
	sess, _ := p.call(t, ctx, "330", offer)
	done := make(chan error, 1)
	go func() { done <- sess.WaitAnswer(ctx, sipgo.AnswerOptions{}) }()
	c := <-ep.calls
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	if e := nextEvent(t, c, sip.EvEnded); e.Code != 487 {
		t.Errorf("cancelled call: %+v", e)
	}

	// Busy: the endpoint rejects.
	ep.mu.Lock()
	ep.action = func(c *sip.Call) { c.Reject(486) }
	ep.mu.Unlock()
	sess, _ = p.call(t, context.Background(), "330", offer)
	var de *sipgo.ErrDialogResponse
	if err := sess.WaitAnswer(context.Background(), sipgo.AnswerOptions{}); !errors.As(err, &de) || de.Res.StatusCode != 486 {
		t.Errorf("reject: %v", err)
	}
	<-ep.calls

	// No common codec: 488 before anything rings.
	_, _, gsm, _ := pbxOffer(t, p, sdp.EncOff, "PCMU")
	a2 := a
	a2.Codecs = []string{"G722"}
	m.SetAccounts([]sip.Account{a2})
	waitFor(t, "re-registration", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegRegistered })
	sess, _ = p.call(t, context.Background(), "330", gsm)
	if err := sess.WaitAnswer(context.Background(), sipgo.AnswerOptions{}); !errors.As(err, &de) || de.Res.StatusCode != 488 {
		t.Errorf("no common codec: %v", err)
	}

	// An unknown contact user gets 404.
	contact, _ := p.registered("330")
	contact.User = "dw-nobody"
	req := esip.NewRequest(esip.INVITE, contact)
	req.SetBody(offer)
	cs, err := p.dialogs.WriteInvite(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.WaitAnswer(context.Background(), sipgo.AnswerOptions{}); !errors.As(err, &de) || de.Res.StatusCode != 404 {
		t.Errorf("unknown user: %v", err)
	}
}

func TestTLSRegistrationVerifiesTheDomain(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["340"] = "secret-340"; p.users["341"] = "secret-341"; p.users["342"] = "secret-342" })
	roots, good := p.listenTLS(t, "pbx.test")
	m := newManagerRoots(t, nil, nil, roots)
	ok := account(p, "340")
	ok.Transport, ok.Registrar, ok.TLSVerify = resolve.TLS, good.String(), true
	m.SetAccounts([]sip.Account{ok})
	waitFor(t, "TLS registration", func() bool {
		st, _ := m.Status(ok.ID)
		if st.LastError != "" {
			t.Logf("state %s: %s", st.State, st.LastError)
		}
		return st.State == sip.RegRegistered
	})
	if st, _ := m.Status(ok.ID); st.Transport != resolve.TLS {
		t.Errorf("transport %s", st.Transport)
	}

	// A certificate for another name is refused; with verification off
	// it is accepted.
	p2 := newPBX(t)
	p2.set(func() { p2.users["341"] = "secret-341"; p2.users["342"] = "secret-342" })
	roots2, wrong := p2.listenTLS(t, "evil.test")
	m2 := newManagerRoots(t, nil, nil, roots2)
	strict := account(p2, "341")
	strict.Transport, strict.Registrar, strict.TLSVerify = resolve.TLS, wrong.String(), true
	m2.SetAccounts([]sip.Account{strict})
	waitFor(t, "TLS refusal", func() bool {
		st, _ := m2.Status(strict.ID)
		return st.State == sip.RegBackoff && strings.Contains(st.LastError, "not valid for")
	})
	m3 := newManagerRoots(t, nil, nil, roots2)
	lax := account(p2, "342")
	lax.Transport, lax.Registrar, lax.TLSVerify = resolve.TLS, wrong.String(), false
	m3.SetAccounts([]sip.Account{lax})
	waitFor(t, "unverified TLS registration", func() bool { st, _ := m3.Status(lax.ID); return st.State == sip.RegRegistered })
}

// A request that knows only the Call-ID of an established call (it travels in
// clear text) is not the far end's: wrong tags or a foreign From or To tag get
// 481, and the call goes on.
func TestInDialogRequestsNeedBothTags(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["325"] = "secret-325" })
	ep := &endpoint{calls: make(chan *sip.Call, 4)}
	ep.action = func(c *sip.Call) { c.Answer() }
	m := newManager(t, ep, nil)
	a := account(p, "325")
	m.SetAccounts([]sip.Account{a})
	waitFor(t, "registration", func() bool { st, _ := m.Status(a.ID); return st.State == sip.RegRegistered })

	_, _, offer, _ := pbxOffer(t, p, sdp.EncOff, "PCMA")
	sess, err := p.call(t, context.Background(), "325", offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.WaitAnswer(context.Background(), sipgo.AnswerOptions{}); err != nil {
		t.Fatal(err)
	}
	sess.Ack(context.Background())
	c := <-ep.calls
	nextEvent(t, c, sip.EvAnswered)

	callID := sess.InviteRequest.CallID().Value()
	fromTag, _ := sess.InviteRequest.From().Params.Get("tag")
	toTag, _ := sess.InviteResponse.To().Params.Get("tag")
	forge := func(method esip.RequestMethod, from, to string) int {
		t.Helper()
		req := esip.NewRequest(method, sess.InviteResponse.Contact().Address)
		cid := esip.CallIDHeader(callID)
		req.AppendHeader(&cid)
		f := &esip.FromHeader{Address: esip.Uri{Scheme: "sip", User: "+4930123456", Host: "pbx.test"}, Params: esip.NewParams()}
		f.Params.Add("tag", from)
		req.AppendHeader(f)
		to2 := &esip.ToHeader{Address: esip.Uri{Scheme: "sip", User: "325", Host: "pbx.test"}, Params: esip.NewParams()}
		to2.Params.Add("tag", to)
		req.AppendHeader(to2)
		req.AppendHeader(&esip.CSeqHeader{SeqNo: 500, MethodName: method})
		switch method {
		case esip.INFO:
			req.SetBody([]byte("Signal=5\r\nDuration=160\r\n"))
			req.AppendHeader(esip.NewHeader("Content-Type", "application/dtmf-relay"))
		}
		tx, err := p.client.TransactionRequest(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Terminate()
		select {
		case res := <-tx.Responses():
			return res.StatusCode
		case <-time.After(3 * time.Second):
			t.Fatal("no answer to the forged request")
		}
		return 0
	}
	for _, c := range []struct{ name, from, to string }{
		{"wrong From tag", "forged", toTag},
		{"wrong To tag", fromTag, "forged"},
		{"both wrong", "forged", "forged-too"},
		{"tags swapped", toTag, fromTag},
	} {
		for _, method := range []esip.RequestMethod{esip.INFO, esip.BYE, esip.INVITE} {
			if got := forge(method, c.from, c.to); got != 481 {
				t.Errorf("%s: %s got %d, want 481", c.name, method, got)
			}
		}
	}
	select {
	case e := <-c.Events():
		t.Fatalf("a forged request changed the call: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
	// The real far end still can.
	if err := sess.Bye(context.Background()); err != nil {
		t.Fatalf("BYE: %v", err)
	}
	nextEvent(t, c, sip.EvEnded)
}

type regReport struct {
	id   uuid.UUID
	st   sip.RegStatus
	took time.Duration
}

// The SIP side tells the history (Config.OnRegChange) when a line's registration state or error changes: a success with the
// time it took, a refusal with its reason; and not when a line is only stopped (removed or replaced).
func TestRegistrationChangesAreReported(t *testing.T) {
	p := newPBX(t)
	p.set(func() { p.users["305"] = "secret-305"; p.users["306"] = "the-right-one" })
	var mu sync.Mutex
	var got []regReport
	m := newManagerHook(t, nil, nil, nil, func(id uuid.UUID, st sip.RegStatus, took time.Duration) {
		mu.Lock()
		got = append(got, regReport{id, st, took})
		mu.Unlock()
	})
	good, bad := account(p, "305"), account(p, "306") // 306: wrong password
	m.SetAccounts([]sip.Account{good, bad})
	waitFor(t, "both settled", func() bool {
		a, _ := m.Status(good.ID)
		b, _ := m.Status(bad.ID)
		return a.State == sip.RegRegistered && b.State == sip.RegFailed
	})
	find := func(id uuid.UUID, state sip.RegState) (regReport, bool) {
		mu.Lock()
		defer mu.Unlock()
		for _, r := range got {
			if r.id == id && r.st.State == state {
				return r, true
			}
		}
		return regReport{}, false
	}
	if r, ok := find(good.ID, sip.RegRegistered); !ok || r.took <= 0 || r.took > 5*time.Second {
		t.Errorf("the success was not reported with its time: %+v %v", r, ok)
	}
	if _, ok := find(good.ID, sip.RegRegistering); !ok {
		t.Error("the attempt was not reported")
	}
	if r, ok := find(bad.ID, sip.RegFailed); !ok || !strings.HasPrefix(r.st.LastError, "401") || strings.Contains(r.st.LastError, "secret") || r.took != 0 {
		t.Errorf("the refusal was not reported with its reason: %+v %v", r, ok)
	}
	// removing the lines stops them: that is not a change of the account's registration
	m.SetAccounts(nil)
	waitFor(t, "unregister", func() bool { c, _ := p.registered("305"); return c.Host == "" })
	time.Sleep(100 * time.Millisecond)
	if _, ok := find(good.ID, sip.RegDisabled); ok {
		t.Error("stopping a line was reported as the account being disabled")
	}
	// a line that does not register is reported as disabled once
	off := account(p, "307")
	off.Register = false
	m.SetAccounts([]sip.Account{off})
	waitFor(t, "off", func() bool { _, ok := find(off.ID, sip.RegDisabled); return ok })
}
