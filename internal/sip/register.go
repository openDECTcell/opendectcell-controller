// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"

	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
)

// RegState is an account's registration state (docs/sip-ua.md §3).
type RegState string

// The registration states. RegDisabled: the account is off or does not register,
// or its line has been closed. RegRegistering: a REGISTER is in flight. RegRegistered: the
// server accepted it, until RegStatus.RegisteredUntil. RegFailed: the server
// or the transport refused it. RegBackoff: waiting before the next attempt after a failure.
const (
	RegDisabled    RegState = "disabled"
	RegRegistering RegState = "registering"
	RegRegistered  RegState = "registered"
	RegFailed      RegState = "failed"
	RegBackoff     RegState = "backoff"
)

// RegStatus is what the API shows (SipAccountStatus).
type RegStatus struct {
	State           RegState
	RegisteredUntil time.Time
	LastError       string // a status line or a transport error; never credentials
	Server          string // the target last used
	Transport       resolve.Transport
}

// Registration timing (docs/sip-ua.md §3). Variables so tests can shorten them.
var (
	backoffMin       = 30 * time.Second
	backoffMax       = 10 * time.Minute
	authFailedRetry  = 10 * time.Minute
	keepaliveEvery   = 30 * time.Second
	requestTimeout   = 8 * time.Second
	unregisterWithin = 2 * time.Second
)

// line is one account's user agent.
type line struct {
	m    *Manager
	acct Account

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	st       RegStatus
	callID   string
	fromTag  string
	cseq     uint32
	public   netip.AddrPort // our address as the registrar saw it (UDP)
	target   resolve.Target // the registrar target that worked last
	hasTgt   bool
	contactU string
	peers    []netip.Addr // the addresses its registrar and proxy resolved to, last time

	// the message-waiting subscription (mwi.go)
	mwiOnce    sync.Once
	mwiKick    chan struct{}
	mwiCallID  string
	mwiFromTag string
	mwiToTag   string
	mwiCSeq    uint32
}

func newLine(m *Manager, a Account) *line {
	return &line{m: m, acct: a, done: make(chan struct{}), callID: sip.GenerateTagN(24) + "@opendectcell",
		fromTag: sip.GenerateTagN(16), cseq: 1, contactU: ContactUser(a.ID),
		mwiKick: make(chan struct{}, 1), mwiCallID: sip.GenerateTagN(24) + "@opendectcell", mwiFromTag: sip.GenerateTagN(16), mwiCSeq: 1}
}

func (l *line) same(a Account) bool { return reflect.DeepEqual(l.acct, a) }

func (l *line) status() RegStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st
}

func (l *line) setStatus(fn func(*RegStatus)) {
	l.mu.Lock()
	fn(&l.st)
	l.mu.Unlock()
}

func (l *line) start() {
	l.ctx, l.cancel = context.WithCancel(l.m.ctx)
	go l.loop()
}

// stop ends the registration loop; with unregister it sends REGISTER with
// Expires: 0 (best effort, briefly).
func (l *line) stop(unregister bool) {
	if l.cancel == nil {
		return
	}
	l.cancel()
	<-l.done
	st := l.status()
	if unregister && st.State == RegRegistered {
		l.mu.Lock()
		t, ok := l.target, l.hasTgt
		l.mu.Unlock()
		if ok {
			ctx, cancel := context.WithTimeout(context.Background(), unregisterWithin)
			_, _ = l.register(ctx, t, 0)
			cancel()
		}
	}
	l.setStatus(func(s *RegStatus) { s.State = RegDisabled })
}

func (l *line) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-l.ctx.Done():
		return false
	}
}

func jitter(d time.Duration) time.Duration {
	return d + time.Duration((rand.Float64()*0.4-0.2)*float64(d)) //nolint:gosec // G404: timing jitter, not a secret
}

// registrarHost is where REGISTER goes: the registrar, else the domain.
func (l *line) registrarHost() (string, int) {
	if l.acct.Registrar != "" {
		h, p, _ := resolve.ParseHostPort(l.acct.Registrar)
		return h, p
	}
	return l.acct.Domain, 0
}

// nextHop is where requests go first: the outbound proxy if set (loose
// routing), else the registrar, else the domain.
func (l *line) nextHop() (string, int) {
	if l.acct.OutboundProxy != "" {
		h, p, _ := resolve.ParseHostPort(l.acct.OutboundProxy)
		return h, p
	}
	return l.registrarHost()
}

func (l *line) resolveHop(ctx context.Context, host string, port int) ([]resolve.Target, error) {
	return resolve.Resolve(ctx, l.m.cfg.DNS, resolve.Request{Host: host, Port: port, Transport: l.acct.Transport})
}

// notePeers remembers the addresses the account's servers resolved to.
func (l *line) notePeers(ts []resolve.Target) {
	peers := make([]netip.Addr, 0, len(ts))
	for _, t := range ts {
		peers = append(peers, t.Addr.Addr().Unmap())
	}
	l.mu.Lock()
	l.peers = peers
	l.mu.Unlock()
}

// fromPeer says whether a request came from an address the account's own
// servers have: its registrar or proxy, as last resolved (for an account that
// does not register, resolved now). src is a request's Source, "ip:port".
func (l *line) fromPeer(src string) bool {
	ap, err := netip.ParseAddrPort(src)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	l.mu.Lock()
	peers := l.peers
	l.mu.Unlock()
	if len(peers) == 0 {
		base := l.ctx
		if base == nil {
			base = context.Background()
		}
		ctx, cancel := context.WithTimeout(base, 2*time.Second)
		defer cancel()
		if ts, err := l.targets(ctx); err == nil {
			l.notePeers(ts)
			l.mu.Lock()
			peers = l.peers
			l.mu.Unlock()
		}
	}
	return slices.Contains(peers, ip)
}

// targets returns where to send, with the last working target first.
func (l *line) targets(ctx context.Context) ([]resolve.Target, error) {
	h, p := l.nextHop()
	ts, err := l.resolveHop(ctx, h, p)
	if err != nil {
		return nil, err
	}
	if ts, err = l.m.filterTargets(ts); err != nil {
		return nil, err
	}
	l.mu.Lock()
	last, ok := l.target, l.hasTgt
	l.mu.Unlock()
	if ok {
		for i, t := range ts {
			if t == last {
				ts[0], ts[i] = ts[i], ts[0]
				break
			}
		}
	}
	return ts, nil
}

func (l *line) loop() {
	defer close(l.done)
	if !l.acct.Enabled || !l.acct.Register {
		l.setStatus(func(s *RegStatus) { *s = RegStatus{State: RegDisabled, Transport: l.acct.Transport} })
		<-l.ctx.Done()
		return
	}
	backoff := backoffMin
	for l.ctx.Err() == nil {
		l.setStatus(func(s *RegStatus) { s.State, s.Transport = RegRegistering, l.acct.Transport })
		ts, err := l.targets(l.ctx)
		if err == nil {
			l.notePeers(ts)
		}
		if err != nil {
			l.fail(RegBackoff, "DNS: "+err.Error())
			if !l.sleep(jitter(backoff)) {
				return
			}
			backoff = min(2*backoff, backoffMax)
			continue
		}
		registered := false
		for _, t := range ts {
			granted, err := l.register(l.ctx, t, l.acct.RegisterExpiry)
			if errors.Is(err, errContactChanged) {
				// The registrar told us our public address: register again
				// with it at once.
				granted, err = l.register(l.ctx, t, l.acct.RegisterExpiry)
			}
			if err == nil {
				registered = true
				backoff = backoffMin
				l.registered(t, granted)
				if !l.holdRegistration(t, granted) {
					return
				}
				break
			}
			var rerr *responseError
			if errors.As(err, &rerr) && (rerr.code == 401 || rerr.code == 403 || rerr.code == 407) {
				l.fail(RegFailed, rerr.Error())
				if !l.sleep(jitter(authFailedRetry)) {
					return
				}
				registered = true // not a target problem: don't try the next one
				break
			}
			l.fail(RegBackoff, err.Error())
			if l.ctx.Err() != nil {
				return
			}
		}
		if !registered {
			if !l.sleep(jitter(backoff)) {
				return
			}
			backoff = min(2*backoff, backoffMax)
		}
	}
}

func (l *line) fail(st RegState, msg string) {
	l.setStatus(func(s *RegStatus) { s.State, s.LastError = st, msg })
	l.m.log.Info("SIP registration problem", "account", l.acct.ID, "state", st, "error", msg)
}

func (l *line) registered(t resolve.Target, granted time.Duration) {
	l.mu.Lock()
	l.target, l.hasTgt = t, true
	l.st = RegStatus{State: RegRegistered, RegisteredUntil: l.m.cfg.Now().Add(granted), Server: t.String(), Transport: t.Transport}
	l.mu.Unlock()
	l.startMWI()
}

// refreshAfter is when to refresh a binding granted for d.
func refreshAfter(d time.Duration) time.Duration {
	r := min(d*8/10, d-30*time.Second)
	if r < d/2 {
		r = d / 2
	}
	return r
}

// holdRegistration waits until the refresh, sending keep-alives meanwhile.
// It returns false when the line is stopping.
func (l *line) holdRegistration(t resolve.Target, granted time.Duration) bool {
	deadline := time.Now().Add(refreshAfter(granted))
	ka := l.keepalive(t)
	for {
		wait := time.Until(deadline)
		if wait <= 0 {
			return true
		}
		if ka {
			wait = min(wait, keepaliveEvery)
		}
		if !l.sleep(wait) {
			return false
		}
		if ka && time.Until(deadline) > 0 {
			l.sendOptions(t)
		}
	}
}

// keepalive decides whether to send OPTIONS keep-alives (docs/sip-ua.md
// §3). "crlf" is treated like "options" for now: sipgo does not expose a
// way to write a bare CRLF on a connection.
func (l *line) keepalive(t resolve.Target) bool {
	switch l.acct.Keepalive {
	case "none":
		return false
	case "options", "crlf":
		return true
	}
	a := t.Addr.Addr()
	return t.Transport == resolve.UDP && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast()
}

func (l *line) sendOptions(t resolve.Target) {
	ctx, cancel := context.WithTimeout(l.ctx, requestTimeout)
	defer cancel()
	req := sip.NewRequest(sip.OPTIONS, sip.Uri{Scheme: "sip", Host: l.acct.Domain})
	l.baseHeaders(req, t, sip.OPTIONS, sip.GenerateTagN(24)+"@opendectcell", sip.GenerateTagN(16), 1)
	if _, err := l.do(ctx, req); err != nil {
		l.m.log.Debug("SIP keep-alive failed", "account", l.acct.ID, "err", err)
	}
}

// contact is our Contact for target.
func (l *line) contact(t resolve.Target) *sip.ContactHeader {
	l.mu.Lock()
	pub := l.public
	l.mu.Unlock()
	host, port := localAddrFor(t.Addr).String(), l.m.contactPort(t.Transport)
	if pub.IsValid() && t.Transport == resolve.UDP {
		host, port = pub.Addr().String(), int(pub.Port())
	}
	c := &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: l.contactU, Host: host, Port: port, UriParams: sip.NewParams()}}
	if t.Transport != resolve.UDP {
		c.Address.UriParams.Add("transport", string(t.Transport))
	}
	return c
}

func (l *line) aor() sip.Uri {
	return sip.Uri{Scheme: "sip", User: l.acct.Username, Host: l.acct.Domain}
}

// baseHeaders adds Via, From, To, Call-ID, CSeq, Max-Forwards, User-Agent,
// Route and the destination for a request to target.
func (l *line) baseHeaders(req *sip.Request, t resolve.Target, method sip.RequestMethod, callID, fromTag string, cseq uint32) {
	req.AppendHeader(l.m.via(t))
	from := &sip.FromHeader{DisplayName: l.acct.DisplayName, Address: l.aor(), Params: sip.NewParams()}
	from.Params.Add("tag", fromTag)
	req.AppendHeader(from)
	if method == sip.REGISTER {
		req.AppendHeader(&sip.ToHeader{Address: l.aor(), Params: sip.NewParams()})
	}
	cid := sip.CallIDHeader(callID)
	req.AppendHeader(&cid)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: method})
	mf := sip.MaxForwardsHeader(70)
	req.AppendHeader(&mf)
	ua := l.acct.UserAgent
	if ua == "" {
		ua = l.m.cfg.UserAgent
	}
	req.AppendHeader(sip.NewHeader("User-Agent", ua))
	if l.acct.OutboundProxy != "" {
		h, p, _ := resolve.ParseHostPort(l.acct.OutboundProxy)
		r := &sip.RouteHeader{Address: sip.Uri{Scheme: "sip", Host: h, Port: p, UriParams: sip.NewParams()}}
		r.Address.UriParams.Add("lr", "")
		if l.acct.Transport != resolve.UDP {
			r.Address.UriParams.Add("transport", string(l.acct.Transport))
		}
		req.AppendHeader(r)
	}
	req.SetTransport(sipTransport(t.Transport))
	req.SetDestination(tlsDestination(t))
	if t.Transport == resolve.TLS {
		// RFC 5922: the certificate must name the SIP domain, or the server the
		// account was configured with. A name that only an SRV record gave
		// (t.Host differs from both) is not trusted: DNS is not authenticated.
		l.m.tlsv.expectFor(l.acct.ID.String(), t.Host, l.acct.TLSVerify, l.acct.Domain, hostOf(l.acct.OutboundProxy), hostOf(l.acct.Registrar))
	}
}

// tlsDestination is where to send: the address, except for TLS to a named
// host, where the name is used so that sipgo verifies and sends SNI for it.
func tlsDestination(t resolve.Target) string {
	if t.Transport == resolve.TLS && tlsKey(t.Host) != "" {
		return net.JoinHostPort(t.Host, strconv.Itoa(int(t.Addr.Port())))
	}
	return t.Addr.String()
}

// responseError is a final non-2xx response.
type responseError struct {
	code   int
	reason string
}

func (e *responseError) Error() string { return strconv.Itoa(e.code) + " " + e.reason }

var errContactChanged = errors.New("sip: contact changed")

// do sends a non-INVITE request and returns its final response.
func (l *line) do(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	tx, err := l.m.client.TransactionRequest(ctx, req, buildRequest)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()
	for {
		select {
		case res := <-tx.Responses():
			if res.IsProvisional() {
				continue
			}
			return res, nil
		case <-tx.Done():
			if err := tx.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("sip: transaction ended without a response")
		case <-ctx.Done():
			return nil, fmt.Errorf("sip: no response from %s: %w", req.Destination(), ctx.Err())
		}
	}
}

// register sends one REGISTER (answering one challenge, two for a stale
// nonce) and returns the granted expiry.
func (l *line) register(ctx context.Context, t resolve.Target, expires int) (time.Duration, error) {
	l.mu.Lock()
	cseq := l.cseq
	l.cseq += 3
	callID, tag := l.callID, l.fromTag
	l.mu.Unlock()
	contact := l.contact(t)
	req := sip.NewRequest(sip.REGISTER, sip.Uri{Scheme: "sip", Host: l.acct.Domain})
	l.baseHeaders(req, t, sip.REGISTER, callID, tag, cseq)
	req.AppendHeader(contact)
	exp := sip.ExpiresHeader(expires)
	req.AppendHeader(&exp)
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS, INFO, NOTIFY"))
	var res *sip.Response
	var err error
	for attempt := 0; ; attempt++ {
		if res, err = l.do(ctx, req); err != nil {
			return 0, err
		}
		if (res.StatusCode != 401 && res.StatusCode != 407) || attempt >= 2 || l.acct.Password == "" {
			break
		}
		if attempt == 1 && !staleNonce(res) {
			break
		}
		req = req.Clone() // the finished transaction keeps the old one
		if err := applyDigest(req, res, l.acct.authUser(), l.acct.Password); err != nil {
			return 0, err
		}
		req.CSeq().SeqNo++
		req.RemoveHeader("Via")
		req.PrependHeader(l.m.via(t))
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return 0, &responseError{res.StatusCode, res.Reason}
	}
	if expires == 0 {
		return 0, nil
	}
	if t.Transport == resolve.UDP {
		if ip, err := netip.ParseAddr(contact.Address.Host); err == nil &&
			l.learnPublic(res, netip.AddrPortFrom(ip, uint16(contact.Address.Port))) {
			return 0, errContactChanged
		}
	}
	return grantedExpiry(res, contact, expires), nil
}

func staleNonce(res *sip.Response) bool {
	for _, n := range []string{"WWW-Authenticate", "Proxy-Authenticate"} {
		for _, h := range res.GetHeaders(n) {
			if strings.Contains(strings.ToLower(h.Value()), "stale=true") {
				return true
			}
		}
	}
	return false
}

// learnPublic records the address the registrar saw us at (Via received
// and rport, RFC 3581). It reports whether that differs from the address
// our Contact advertised, in which case we register again with it.
func (l *line) learnPublic(res *sip.Response, advertised netip.AddrPort) bool {
	v := res.Via()
	if v == nil {
		return false
	}
	recv, ok1 := v.Params.Get("received")
	rp, ok2 := v.Params.Get("rport")
	if !ok1 || !ok2 {
		return false
	}
	ip, err := netip.ParseAddr(recv)
	port, perr := strconv.Atoi(rp)
	if err != nil || perr != nil || port < 1 || port > 65535 {
		return false
	}
	seen := netip.AddrPortFrom(ip.Unmap(), uint16(port))
	if seen == advertised {
		return false
	}
	l.mu.Lock()
	l.public = seen
	l.mu.Unlock()
	return true
}

// grantedExpiry reads what the registrar granted: our Contact's expires
// parameter, else the Expires header, else what we asked for.
func grantedExpiry(res *sip.Response, ours *sip.ContactHeader, asked int) time.Duration {
	for _, h := range res.GetHeaders("Contact") {
		c, ok := h.(*sip.ContactHeader)
		if !ok {
			continue
		}
		if c.Address.User == ours.Address.User {
			if e, ok := c.Params.Get("expires"); ok {
				if n, err := strconv.Atoi(e); err == nil && n > 0 {
					return time.Duration(n) * time.Second
				}
			}
		}
	}
	if h := res.GetHeader("Expires"); h != nil {
		if n, err := strconv.Atoi(h.Value()); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(asked) * time.Second
}

// hostOf is the name in a host[:port] setting, or "" for nothing or an IP
// address (an address names no certificate identity).
func hostOf(hp string) string {
	if hp == "" {
		return ""
	}
	h, _, err := resolve.ParseHostPort(hp)
	if err != nil || h == "" {
		return ""
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return ""
	}
	return h
}
