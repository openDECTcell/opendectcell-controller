// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

// Direction of a call.
type Direction string

const (
	Outgoing Direction = "out"
	Incoming Direction = "in"
)

// EventKind is what happened on a call.
type EventKind string

const (
	// EvRinging is a 180 from the far end (outgoing calls). It can be
	// missing when the far end answers at once: sipgo passes only the
	// latest response up, so a 200 right behind a 180 overtakes it.
	EvRinging    EventKind = "ringing"
	EvEarlyMedia EventKind = "early_media" // 183 with SDP: play the network's ringback
	EvAnswered   EventKind = "answered"
	EvHeld       EventKind = "held"    // the far end put us on hold
	EvResumed    EventKind = "resumed" // and took us off it
	EvDTMF       EventKind = "dtmf"    // a digit from the far end
	EvEnded      EventKind = "ended"
)

// Event is one call event. For EvEnded, Code is the SIP status that ended
// the call (0 for a normal BYE) and Reason a short text.
type Event struct {
	Kind   EventKind
	Code   int
	Reason string
	Digit  rune
}

// Call is one SIP call, in either direction.
type Call struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	Direction Direction
	// Remote is the far party: the dialled number, or the caller's
	// identity (P-Asserted-Identity, else From, per the account).
	Remote        string
	RemoteDisplay string

	m      *Manager
	l      *line
	callID string
	events chan Event
	media  *rtp.Endpoint
	local  sdp.Local
	keys   []sdp.Crypto
	target resolve.Target

	mu       sync.Mutex
	neg      *sdp.Media
	client   *sipgo.DialogClientSession
	server   *sipgo.DialogServerSession
	answered bool
	ended    bool
	heldByUs bool
	cancel   context.CancelFunc
	sdpVer   uint64
	// pendingAnswer is the SDP answer to an incoming INVITE, sent with 200.
	pendingAnswer []byte
	// done is closed when the call has ended.
	done chan struct{}
	// endReason, if set, is why the dialog is ending (our own hangup).
	endReason string
}

// Done is closed when the call has ended.
func (c *Call) Done() <-chan struct{} { return c.done }

// Events delivers the call's events. It is closed after EvEnded.
func (c *Call) Events() <-chan Event { return c.events }

// Media is the call's RTP endpoint, for the media engine (C5).
func (c *Call) Media() *rtp.Endpoint { return c.media }

// Negotiated returns the agreed media, or nil before an answer.
func (c *Call) Negotiated() *sdp.Media {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.neg
}

func (c *Call) emit(e Event) {
	// The send is non-blocking, so it can happen under the lock; that keeps
	// a late event from racing the close below.
	c.mu.Lock()
	if c.ended {
		c.mu.Unlock()
		return
	}
	select {
	case c.events <- e:
	default:
		c.m.log.Warn("call event dropped: consumer too slow", "call", c.ID, "event", e.Kind)
	}
	if e.Kind != EvEnded {
		c.mu.Unlock()
		return
	}
	c.ended = true
	close(c.events)
	close(c.done)
	c.mu.Unlock()
	c.media.Close()
	c.m.removeCall(c)
}

func (m *Manager) newCall(l *line, dir Direction, callID string, ep *rtp.Endpoint, t resolve.Target) *Call {
	c := &Call{ID: uuid.New(), AccountID: l.acct.ID, Direction: dir, m: m, l: l, callID: callID,
		events: make(chan Event, 32), media: ep, target: t, sdpVer: 1, done: make(chan struct{})}
	mediaIP := m.cfg.MediaIP
	if !mediaIP.IsValid() {
		mediaIP = localAddrFor(t.Addr)
	}
	c.local = sdp.Local{Addr: netip.AddrPortFrom(mediaIP, uint16(ep.LocalPort())), Codecs: l.acct.Codecs, Encryption: l.acct.Encryption}
	ep.OnDTMF = func(d rune, _ time.Duration) { c.emit(Event{Kind: EvDTMF, Digit: d}) }
	m.addCall(c)
	return c
}

// ErrNoAccount means the account is unknown or disabled.
var ErrNoAccount = errors.New("sip: account not available")

// Dial places a call to number on an account. The call proceeds in the
// background; follow it on Events.
func (m *Manager) Dial(ctx context.Context, accountID uuid.UUID, number string) (*Call, error) {
	l := m.line(accountID)
	if l == nil || !l.acct.Enabled {
		return nil, ErrNoAccount
	}
	number = strings.TrimSpace(number)
	if number == "" || strings.ContainsAny(number, " \t\r\n<>;@\"") {
		return nil, errorf("invalid number %q", number)
	}
	ts, err := l.targets(ctx)
	if err != nil {
		return nil, err
	}
	ep, err := m.cfg.RTP.Open(ctx)
	if err != nil {
		return nil, err
	}
	c := m.newCall(l, Outgoing, sip.GenerateTagN(24)+"@opendectcell", ep, ts[0])
	c.Remote = number
	cctx, cancel := context.WithCancel(m.ctx)
	c.cancel = cancel
	go c.runOutgoing(cctx, number, ts)
	return c, nil
}

func (c *Call) offer(dir sdp.Direction) ([]byte, error) {
	c.mu.Lock()
	l := c.local
	l.Direction = dir
	l.Version = c.sdpVer
	c.sdpVer++
	c.mu.Unlock()
	body, keys, err := sdp.Offer(l)
	if err == nil {
		c.mu.Lock()
		c.keys = keys
		c.mu.Unlock()
	}
	return body, err
}

func (c *Call) buildInvite(number string, t resolve.Target, body []byte, fromTag string, cseq uint32) *sip.Request {
	uri := sip.Uri{Scheme: "sip", User: number, Host: c.l.acct.Domain}
	req := sip.NewRequest(sip.INVITE, uri)
	c.l.baseHeaders(req, t, sip.INVITE, c.callID, fromTag, cseq)
	req.AppendHeader(&sip.ToHeader{Address: uri, Params: sip.NewParams()})
	req.AppendHeader(c.l.contact(t))
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS, INFO, NOTIFY"))
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(body)
	return req
}

func (c *Call) runOutgoing(ctx context.Context, number string, ts []resolve.Target) {
	body, err := c.offer(sdp.SendRecv)
	if err != nil {
		c.emit(Event{Kind: EvEnded, Code: 500, Reason: err.Error()})
		return
	}
	fromTag := sip.GenerateTagN(16)
	cseq := uint32(1)
	var lastErr error
	for i := 0; i < len(ts); i++ {
		t := ts[i]
		c.target = t
		req := c.buildInvite(number, t, body, fromTag, cseq)
		var res *sip.Response
		for auth := 0; auth <= 2; auth++ {
			res, err = c.invite(ctx, req)
			if err != nil || res == nil || (res.StatusCode != 401 && res.StatusCode != 407) || c.l.acct.Password == "" {
				break
			}
			if auth == 1 && !staleNonce(res) {
				break
			}
			// A new request, not the old one changed: the first
			// transaction still builds its ACK for the 401/407 from it.
			req = req.Clone()
			if derr := applyDigest(req, res, c.l.acct.authUser(), c.l.acct.Password); derr != nil {
				err = derr
				break
			}
			cseq++
			req.CSeq().SeqNo = cseq
			req.RemoveHeader("Via")
			req.PrependHeader(c.m.via(t))
		}
		if errors.Is(err, context.Canceled) {
			c.emit(Event{Kind: EvEnded, Code: 487, Reason: "cancelled"})
			return
		}
		if err != nil {
			// A transport failure before any answer: next target.
			lastErr = err
			continue
		}
		if res == nil {
			c.answeredOut(ctx)
			return
		}
		if res.StatusCode == 503 && i+1 < len(ts) {
			continue // RFC 3263 §4.3: try the next server
		}
		c.emit(Event{Kind: EvEnded, Code: res.StatusCode, Reason: res.Reason})
		return
	}
	reason := "no server reachable"
	if lastErr != nil {
		reason = lastErr.Error()
	}
	c.emit(Event{Kind: EvEnded, Code: 503, Reason: reason})
}

// invite sends one INVITE and waits. It returns (nil, nil) when answered,
// the final response when not, or an error for transport failures and
// cancellation. On cancellation it sends CANCEL itself: sipgo's CANCEL
// would not go to the INVITE's destination.
func (c *Call) invite(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	sess, err := c.m.dialogs.WriteInvite(context.WithoutCancel(ctx), req, buildRequest)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.client = sess
	c.mu.Unlock()
	wctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	go func() {
		select {
		case <-ctx.Done():
			stop(sipgo.WaitAnswerForceCancelErr)
		case <-wctx.Done():
		}
	}()
	var provisional bool
	err = sess.WaitAnswer(wctx, sipgo.AnswerOptions{OnResponse: func(res *sip.Response) error {
		provisional = provisional || res.IsProvisional()
		switch {
		case res.StatusCode == 180:
			c.emit(Event{Kind: EvRinging, Code: 180})
		case res.StatusCode == 183 && len(res.Body()) > 0:
			if m, err := sdp.ParseAnswer(res.Body(), c.local, c.keys); err == nil {
				if err := c.media.Configure(m); err == nil {
					c.setNeg(m)
					c.emit(Event{Kind: EvEarlyMedia, Code: 183})
				}
			}
		}
		return nil
	}})
	var derr *sipgo.ErrDialogResponse
	switch {
	case err == nil:
		return nil, nil
	case errors.As(err, &derr):
		return derr.Res, nil
	case ctx.Err() != nil:
		if provisional {
			c.sendCancel(req)
		}
		return nil, context.Canceled
	}
	return nil, err
}

// sendCancel cancels an INVITE (RFC 3261 §9.1): same Request-URI, Call-ID,
// From, To, CSeq number and top Via, to the same destination.
func (c *Call) sendCancel(inv *sip.Request) {
	cr := sip.NewRequest(sip.CANCEL, inv.Recipient)
	cr.AppendHeader(sip.HeaderClone(inv.Via()))
	cr.AppendHeader(sip.HeaderClone(inv.From()))
	cr.AppendHeader(sip.HeaderClone(inv.To()))
	cr.AppendHeader(sip.HeaderClone(inv.CallID()))
	cr.AppendHeader(&sip.CSeqHeader{SeqNo: inv.CSeq().SeqNo, MethodName: sip.CANCEL})
	mf := sip.MaxForwardsHeader(70)
	cr.AppendHeader(&mf)
	sip.CopyHeaders("Route", inv, cr)
	cr.SetTransport(inv.Transport())
	cr.SetDestination(inv.Destination())
	cr.Laddr = inv.Laddr
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	tx, err := c.m.client.TransactionRequest(ctx, cr, func(*sipgo.Client, *sip.Request) error { return nil })
	if err != nil {
		c.m.log.Debug("CANCEL failed", "call", c.ID, "err", err)
		return
	}
	defer tx.Terminate()
	select {
	case <-tx.Responses():
	case <-tx.Done():
	case <-ctx.Done():
	}
}

func (c *Call) setNeg(m *sdp.Media) {
	c.mu.Lock()
	c.neg = m
	c.mu.Unlock()
}

func (c *Call) answeredOut(ctx context.Context) {
	c.mu.Lock()
	sess := c.client
	c.mu.Unlock()
	m, err := sdp.ParseAnswer(sess.InviteResponse.Body(), c.local, c.keys)
	if err == nil {
		err = c.media.Configure(m)
	}
	if aerr := sess.Ack(ctx); aerr != nil && err == nil {
		err = aerr
	}
	if err != nil {
		// RFC 3264 §5: an answer we can't use is ended with BYE.
		c.bye(ctx)
		c.emit(Event{Kind: EvEnded, Code: 488, Reason: err.Error()})
		return
	}
	c.mu.Lock()
	c.neg, c.answered = m, true
	c.mu.Unlock()
	c.emit(Event{Kind: EvAnswered, Code: 200})
	go c.watch(sess.Context())
}

// watch ends the call when its dialog ends (BYE from the far end, or a
// transaction failure).
func (c *Call) watch(ctx context.Context) {
	<-ctx.Done()
	c.mu.Lock()
	reason := c.endReason
	c.mu.Unlock()
	if reason == "" {
		reason = "dialog ended"
	}
	c.emit(Event{Kind: EvEnded, Reason: reason})
}

// incoming

func (l *line) incoming(req *sip.Request, tx sip.ServerTransaction) {
	m := l.m
	if !l.acct.Enabled {
		m.respond(tx, req, 480, "Temporarily Unavailable")
		return
	}
	sess, err := m.dialogs.ReadInvite(req, tx)
	if err != nil {
		m.respond(tx, req, 400, "Bad Request")
		return
	}
	src, err := netip.ParseAddrPort(req.Source())
	if err != nil {
		m.respond(tx, req, 400, "Bad Request")
		return
	}
	t := resolve.Target{Transport: resolve.Transport(strings.ToLower(req.Transport())), Addr: netip.AddrPortFrom(src.Addr().Unmap(), src.Port())}
	ep, err := m.cfg.RTP.Open(context.Background())
	if err != nil {
		_ = sess.Respond(503, "Service Unavailable", nil)
		return
	}
	c := m.newCall(l, Incoming, req.CallID().Value(), ep, t)
	c.server = sess
	c.Remote, c.RemoteDisplay = callerID(req, l.acct.CallerIDSource)
	// Answer the offer now, so a 488 goes out before anything rings.
	ans, neg, err := sdp.Answer(req.Body(), c.local)
	if err != nil {
		_ = sess.Respond(488, "Not Acceptable Here", nil)
		c.emit(Event{Kind: EvEnded, Code: 488, Reason: err.Error()})
		return
	}
	if err := ep.Configure(neg); err != nil {
		_ = sess.Respond(488, "Not Acceptable Here", nil)
		c.emit(Event{Kind: EvEnded, Code: 488, Reason: err.Error()})
		return
	}
	c.setNeg(neg)
	c.pendingAnswer = ans
	// The far end cancelling, or the transaction dying, ends the call.
	go func() {
		<-sess.Context().Done()
		c.mu.Lock()
		answered := c.answered
		c.mu.Unlock()
		if !answered {
			c.emit(Event{Kind: EvEnded, Code: 487, Reason: "cancelled by caller"})
		} else {
			c.mu.Lock()
			reason := c.endReason
			c.mu.Unlock()
			if reason == "" {
				reason = "dialog ended"
			}
			c.emit(Event{Kind: EvEnded, Reason: reason})
		}
	}()
	if m.endpoint == nil {
		c.Reject(480)
		return
	}
	m.endpoint.IncomingCall(c)
	// sipgo ends the server transaction when the INVITE handler returns,
	// so the handler stays until the call is over.
	<-c.done
}

// callerID picks the far party's identity (voip-requirements.md §2).
func callerID(req *sip.Request, source string) (user, display string) {
	pick := func(name string) (string, string, bool) {
		h := req.GetHeader(name)
		if h == nil {
			return "", "", false
		}
		var u sip.Uri
		v := h.Value()
		disp := ""
		if i := strings.IndexByte(v, '<'); i >= 0 {
			disp = strings.Trim(strings.TrimSpace(v[:i]), `"`)
			if j := strings.IndexByte(v[i:], '>'); j > 0 {
				v = v[i+1 : i+j]
			}
		}
		if err := sip.ParseUri(v, &u); err != nil || u.User == "" {
			return "", "", false
		}
		return u.User, disp, true
	}
	order := map[string][]string{"auto": {"P-Asserted-Identity", "Remote-Party-ID", "From"}, "pai": {"P-Asserted-Identity", "From"},
		"rpid": {"Remote-Party-ID", "From"}, "from": {"From"}}[source]
	if order == nil {
		order = []string{"P-Asserted-Identity", "Remote-Party-ID", "From"}
	}
	for _, n := range order {
		if u, d, ok := pick(n); ok {
			return u, d
		}
	}
	return "", ""
}

// Ringing tells the caller the handset is alerting (180).
func (c *Call) Ringing() error {
	c.mu.Lock()
	s := c.server
	c.mu.Unlock()
	if s == nil {
		return errors.New("sip: not an incoming call")
	}
	return s.Respond(180, "Ringing", nil)
}

// Answer accepts an incoming call (200 with the SDP answer). The answer is
// sent in the background; EvAnswered follows when the caller's ACK
// arrives.
func (c *Call) Answer() error {
	c.mu.Lock()
	s, ans := c.server, c.pendingAnswer
	if s == nil || c.answered {
		c.mu.Unlock()
		return errors.New("sip: cannot answer this call")
	}
	c.answered = true
	c.mu.Unlock()
	go func() {
		res := sip.NewSDPResponseFromRequest(s.InviteRequest, ans)
		res.AppendHeader(c.l.contact(c.target))
		if err := s.WriteResponse(res); err != nil {
			c.emit(Event{Kind: EvEnded, Code: 500, Reason: "answer failed: " + err.Error()})
			return
		}
		c.emit(Event{Kind: EvAnswered, Code: 200})
	}()
	return nil
}

// Reject declines an incoming call with a final status (486 busy, 480,
// 603 decline).
func (c *Call) Reject(code int) {
	c.mu.Lock()
	s, answered := c.server, c.answered
	c.mu.Unlock()
	if s == nil || answered {
		return
	}
	reason := map[int]string{480: "Temporarily Unavailable", 486: "Busy Here", 503: "Service Unavailable", 603: "Decline"}[code]
	if reason == "" {
		reason = "Rejected"
	}
	_ = s.Respond(code, reason, nil)
	c.emit(Event{Kind: EvEnded, Code: code, Reason: reason})
}

func (c *Call) readAck(req *sip.Request, tx sip.ServerTransaction) {
	c.mu.Lock()
	s := c.server
	c.mu.Unlock()
	if s != nil {
		_ = s.ReadAck(req, tx)
	}
}

func (c *Call) readBye(req *sip.Request, tx sip.ServerTransaction) {
	c.mu.Lock()
	cs, ss := c.client, c.server
	c.mu.Unlock()
	switch {
	case ss != nil:
		_ = ss.ReadBye(req, tx)
	case cs != nil:
		_ = cs.ReadBye(req, tx)
	default:
		c.m.respond(tx, req, 481, "Call/Transaction Does Not Exist")
	}
	c.mu.Lock()
	if c.endReason == "" {
		c.endReason = "remote hangup"
	}
	c.mu.Unlock()
	c.emit(Event{Kind: EvEnded, Reason: "remote hangup"})
}

// Hangup ends the call: CANCEL before an answer (outgoing), 480 to an
// unanswered incoming call, BYE after.
func (c *Call) Hangup() {
	c.mu.Lock()
	cs, ss, answered, cancel := c.client, c.server, c.answered, c.cancel
	if answered && c.endReason == "" {
		c.endReason = "local hangup"
	}
	c.mu.Unlock()
	ctx, done := context.WithTimeout(context.Background(), requestTimeout)
	defer done()
	switch {
	case c.Direction == Outgoing && !answered:
		if cancel != nil {
			cancel() // WaitAnswer sends CANCEL
		}
		return
	case c.Direction == Incoming && !answered:
		c.Reject(480)
		return
	case cs != nil, ss != nil:
		c.bye(ctx)
	}
	c.emit(Event{Kind: EvEnded, Reason: "local hangup"})
}

// bye sends BYE with our Via and Contact (sipgo's own BYE would carry its
// empty default Contact).
func (c *Call) bye(ctx context.Context) {
	c.mu.Lock()
	cs, ss := c.client, c.server
	c.mu.Unlock()
	bye := sip.NewRequest(sip.BYE, c.remoteTarget())
	bye.AppendHeader(c.m.via(c.target))
	bye.AppendHeader(c.l.contact(c.target))
	switch {
	case cs != nil:
		_ = cs.WriteBye(ctx, bye)
	case ss != nil:
		_ = ss.WriteBye(ctx, bye)
	}
}

// in-dialog requests

// dialogRequest sends a request inside the call's dialog.
func (c *Call) dialogRequest(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	c.mu.Lock()
	cs, ss := c.client, c.server
	c.mu.Unlock()
	req.AppendHeader(c.m.via(c.target))
	req.AppendHeader(c.l.contact(c.target))
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	switch {
	case cs != nil:
		return cs.Do(ctx, req)
	case ss != nil:
		return ss.Do(ctx, req)
	}
	return nil, errors.New("sip: no dialog")
}

func (c *Call) remoteTarget() sip.Uri {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil && c.client.InviteResponse != nil {
		if ct := c.client.InviteResponse.Contact(); ct != nil {
			return ct.Address
		}
	}
	if c.server != nil {
		if ct := c.server.InviteRequest.Contact(); ct != nil {
			return ct.Address
		}
	}
	return sip.Uri{}
}

// Hold puts the call on hold (re-INVITE with sendonly) or resumes it
// (sendrecv).
func (c *Call) Hold(ctx context.Context, on bool) error {
	dir := sdp.SendRecv
	if on {
		dir = sdp.SendOnly
	}
	body, err := c.offer(dir)
	if err != nil {
		return err
	}
	req := sip.NewRequest(sip.INVITE, c.remoteTarget())
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(body)
	res, err := c.dialogRequest(ctx, req)
	if err != nil {
		return err
	}
	if res.StatusCode != 200 {
		return &responseError{res.StatusCode, res.Reason}
	}
	ack := sip.NewRequest(sip.ACK, c.remoteTarget())
	ack.AppendHeader(c.m.via(c.target))
	c.mu.Lock()
	cs, ss := c.client, c.server
	c.mu.Unlock()
	cseq := req.CSeq().SeqNo
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: sip.ACK})
	if cs != nil {
		err = cs.WriteRequest(ack)
	} else if ss != nil {
		err = ss.WriteRequest(ack)
	}
	if err != nil {
		return err
	}
	l := c.local
	l.Direction = dir
	m, perr := sdp.ParseAnswer(res.Body(), l, c.keys)
	if perr == nil {
		perr = c.media.Configure(m)
		c.setNeg(m)
	}
	c.mu.Lock()
	c.heldByUs = on
	c.mu.Unlock()
	return perr
}

// readReinvite answers a re-INVITE from the far end: hold, resume or a
// media change.
func (c *Call) readReinvite(req *sip.Request, tx sip.ServerTransaction) {
	c.mu.Lock()
	l := c.local
	if c.heldByUs {
		l.Direction = sdp.SendOnly
	}
	l.Version = c.sdpVer
	c.sdpVer++
	prev := c.neg
	c.mu.Unlock()
	ans, neg, err := sdp.Answer(req.Body(), l)
	if err != nil {
		c.m.respond(tx, req, 488, "Not Acceptable Here")
		return
	}
	if err := c.media.Configure(neg); err != nil {
		c.m.respond(tx, req, 488, "Not Acceptable Here")
		return
	}
	c.setNeg(neg)
	res := sip.NewSDPResponseFromRequest(req, ans)
	res.AppendHeader(c.l.contact(c.target))
	if err := tx.Respond(res); err != nil {
		return
	}
	switch {
	case neg.HeldByPeer && (prev == nil || !prev.HeldByPeer):
		c.emit(Event{Kind: EvHeld})
	case !neg.HeldByPeer && prev != nil && prev.HeldByPeer:
		c.emit(Event{Kind: EvResumed})
	}
}

// SendDTMF sends a digit the handset pressed, in the account's DTMF mode.
// In-band DTMF is generated by the media engine (C5) and is not handled
// here.
func (c *Call) SendDTMF(ctx context.Context, digit rune) error {
	const dur = 160 * time.Millisecond
	switch c.l.acct.DTMFMode {
	case "info":
		req := sip.NewRequest(sip.INFO, c.remoteTarget())
		req.AppendHeader(sip.NewHeader("Content-Type", "application/dtmf-relay"))
		req.SetBody([]byte(fmt.Sprintf("Signal=%c\r\nDuration=%d\r\n", digit, dur.Milliseconds())))
		res, err := c.dialogRequest(ctx, req)
		if err != nil {
			return err
		}
		if res.StatusCode != 200 {
			return &responseError{res.StatusCode, res.Reason}
		}
		return nil
	case "inband":
		return errors.New("sip: in-band DTMF is generated by the media engine (C5)")
	}
	return c.media.SendDTMF(ctx, digit, dur)
}

// readInfo takes DTMF from SIP INFO (application/dtmf-relay or
// application/dtmf) and answers other INFO with 200.
func (c *Call) readInfo(req *sip.Request, tx sip.ServerTransaction) {
	ct := ""
	if h := req.ContentType(); h != nil {
		ct = strings.ToLower(h.Value())
	}
	body := string(req.Body())
	var digit rune
	switch {
	case strings.HasPrefix(ct, "application/dtmf-relay"):
		for _, ln := range strings.Split(body, "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(ln), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "signal") {
				if v = strings.TrimSpace(v); len(v) == 1 {
					digit = rune(v[0])
				} else if n, err := strconv.Atoi(v); err == nil && n == 10 {
					digit = '*'
				} else if n == 11 {
					digit = '#'
				}
			}
		}
	case strings.HasPrefix(ct, "application/dtmf"):
		if v := strings.TrimSpace(body); len(v) == 1 {
			digit = rune(v[0])
		}
	}
	c.m.respond(tx, req, 200, "OK")
	if digit != 0 && strings.ContainsRune("0123456789*#ABCD", digit) {
		c.emit(Event{Kind: EvDTMF, Digit: digit})
	}
}
