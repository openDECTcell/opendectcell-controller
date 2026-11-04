// SPDX-License-Identifier: AGPL-3.0-or-later

package bridge

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/core"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

// The pieces the call manager joins; the production ones are sip.Manager,
// core.Core, cell.Server, calllog.Store and PGLines, through the small adapters
// below (DialerOf, RouterOf, Outgoing).
type (
	// SIPCall is what the manager needs of a SIP call (*sip.Call).
	SIPCall interface {
		Events() <-chan sip.Event
		Negotiated() *sdp.Media
		Media() *rtp.Endpoint
		Ringing() error
		Answer() error
		Reject(code int)
		Hangup()
		SendDTMF(ctx context.Context, digit rune) error
		Hold(ctx context.Context, on bool) error
	}
	// NameLookup gives the name a handset knows a number under (*contacts.Store); "" for none.
	NameLookup interface {
		LookupName(ctx context.Context, handset uuid.UUID, number string) (string, error)
	}
	// Caller places SIP calls.
	Caller interface {
		Dial(ctx context.Context, account uuid.UUID, number string) (SIPCall, error)
	}
	// Offer is a call offered to a handset (*core.Offer).
	Offer interface {
		Events() <-chan core.CallEvent
		Cancel()
	}
	// Router offers a call to a handset where it is (core.Core).
	Router interface {
		Ring(ctx context.Context, hid uuid.UUID, call core.Call) (Offer, error)
	}
	// Handset is a call a handset placed (*core.Outgoing).
	Handset interface {
		Info() core.OutCall
		Events() <-chan core.CallEvent
		Ringing()
		Answered(narrowband bool)
		Release(reason string)
	}
	// Cells is the cell link's media side (cell.Server).
	Cells interface {
		OpenMedia(id uuid.UUID, m wire.MediaOpenMsg) error
		CloseMedia(id uuid.UUID, mediaID uint32) error
		CellStatus(id uuid.UUID) cell.Status
	}
	// CallLog records calls (calllog.Store); nil Manager.CallLog: none.
	CallLog interface {
		Start(ctx context.Context, c calllog.Call) (int64, error)
		Answered(ctx context.Context, id int64, at time.Time) error
		Ended(ctx context.Context, id int64, e calllog.End) error
	}
)

type dialerOf struct{ d Dialer }

// Dial places an outgoing call through the wrapped Dialer. It converts the
// concrete call to the SIPCall interface only on success, so an error returns
// a nil interface and not a typed nil.
func (x dialerOf) Dial(ctx context.Context, account uuid.UUID, number string) (SIPCall, error) {
	c, err := x.d.Dial(ctx, account, number)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// DialerOf makes a Caller of a Dialer (sip.Manager).
func DialerOf(d Dialer) Caller { return dialerOf{d} }

type offerOf struct{ o *core.Offer }

// Events returns the offer's event channel (see core.Offer).
func (x offerOf) Events() <-chan core.CallEvent { return x.o.Events }

// Cancel is the SIP caller hanging up: the legs still ringing are released.
func (x offerOf) Cancel() { x.o.Cancel() }

type routerOf struct{ c *core.Core }

// Ring rings handset hid for an incoming call and returns the offer; see
// core.Core.Ring. On error it returns a nil Offer, not a typed nil.
func (x routerOf) Ring(ctx context.Context, hid uuid.UUID, call core.Call) (Offer, error) {
	o, err := x.c.Ring(ctx, hid, call)
	if err != nil {
		return nil, err
	}
	return offerOf{o}, nil
}

// RouterOf makes a Router of the DECT core.
func RouterOf(c *core.Core) Router { return routerOf{c} }

type outgoingOf struct{ o *core.Outgoing }

// Info returns what the cell's stack reported of the call: cell, handset,
// line, dialled number and whether the handset's codec is wideband.
func (x outgoingOf) Info() core.OutCall { return x.o.OutCall }

// Events returns the channel of what happens to the call on the DECT side.
func (x outgoingOf) Events() <-chan core.CallEvent { return x.o.Events }

// Ringing tells the handset that the far end rings (the ring-back tone).
func (x outgoingOf) Ringing() { x.o.Ringing() }

// Answered tells the handset that the far end answered; narrowband is true when
// the far end took a narrowband codec, so the handset's leg is narrowband too and
// nothing is transcoded.
func (x outgoingOf) Answered(narrowband bool) { x.o.Answered(narrowband) }

// Release ends the call from the far side; reason is busy, normal, unreachable
// or rejected. The Events channel closes.
func (x outgoingOf) Release(reason string) { x.o.Release(reason) }

// Manager runs the calls between handsets and SIP: a handset's call goes out
// on its line's account (HandsetCall, set as core.Core.Originate), a call to an
// account rings the handsets that have it as a line (IncomingCall, the
// sip.Endpoint), and once a call is answered its speech flows between the
// SIP peer and the cell that holds the handset (media.go).
type Manager struct {
	SIP   Caller
	DECT  Router
	Cells Cells
	Lines Lines
	// Identity says who a handset is in an internal call (*core.Core); nil: internal calls are refused.
	Identity InternalIdentities
	MWI      MWISender // nil: message waiting is not shown (mwi.go)
	// CallLists is told which handsets' call lists a finished call is in (*core.Core); nil: nobody.
	CallLists CallListNotifier
	// Names, if set, gives the name of a caller from the contacts of the handset that rings; it wins over the name
	// the other side sent.
	Names   NameLookup
	CallLog CallLog
	Log     *slog.Logger

	// MediaIP is the address the cells send their speech to, MediaPorts the
	// UDP ports the controller listens on for it (inclusive; 0: any port).
	MediaIP    netip.Addr
	MediaPorts [2]int
	// MediaFromLink: send each cell to the address it connected to (Status.LocalAddr) instead of MediaIP.
	// Right when the controller sees the cells' real connections, as in the host network, and for cells
	// on different networks (a LAN address for one, a public IPv6 address for another).
	MediaFromLink bool

	// Timeouts, defaults below; tests shorten them.
	OpenTimeout time.Duration

	mu       sync.Mutex
	nextID   uint32
	pending  map[uint32]pendingOpen
	sessions map[uint32]*mediaSession
	mwi      map[uuid.UUID]uint // new messages in an account's mailbox, as it last said
}

// CallListNotifier takes the news that a call ended, for the handsets whose call lists it is in.
type CallListNotifier interface {
	CallListsChanged(hids ...uuid.UUID)
}

const defaultOpenTimeout = 3 * time.Second

func (m *Manager) log() *slog.Logger {
	if m.Log == nil {
		return slog.Default()
	}
	return m.Log
}

// reasonOf maps how a SIP call ended to the reason a stack understands
// (remote-released): busy, normal, unreachable or rejected.
func reasonOf(ev sip.Event) string {
	switch ev.Code {
	case 0, 200, 487:
		return "normal"
	case 486, 600:
		return "busy"
	case 603, 403, 401, 407:
		return "rejected"
	}
	return "unreachable"
}

// narrow says the SIP leg is narrowband: its codec is not G.722.
func narrow(c SIPCall) bool {
	n := c.Negotiated()
	return n != nil && n.Codec.Name != "G722"
}

// HandsetCall takes a call a handset placed: it dials the number on the
// handset's line and follows both sides to the end. It is core.Core.Originate.
func (m *Manager) HandsetCall(o *core.Outgoing) { m.handsetCall(outgoingOf{o}) }

func (m *Manager) handsetCall(o Handset) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	info := o.Info()
	if info.Internal {
		m.internalCall(o)
		return
	}
	log := m.log().With("cell", info.Cell, "call", info.Call, "line", info.Line)
	acct, ok, err := m.Lines.OutLine(ctx, info.Handset, info.Line)
	if err != nil || !ok {
		log.Warn("a handset's call has no line to go out on", "handset", info.Handset, "err", err)
		o.Release("unreachable")
		return
	}
	sc, err := m.SIP.Dial(ctx, acct, info.Number)
	if err != nil {
		log.Warn("a handset's call could not be dialled", "err", err)
		o.Release("unreachable")
		return
	}
	log.Info("a handset's call is dialled", "account", acct, "wideband", info.Wideband)
	line := info.Line
	cl := m.logCall(ctx, calllog.Call{HandsetID: idPtr(info.Handset), Line: &line, SIPAccountID: &acct, Direction: "out",
		RemoteParty: info.Number, CellIDs: []uuid.UUID{info.Cell}})
	a := &active{m: m, sip: sc, log: log, calllog: cl, hids: []uuid.UUID{info.Handset}}
	defer a.finish(ctx)
	rang := false
	for {
		select {
		case ev, ok := <-sc.Events():
			if !ok {
				o.Release("normal")
				return
			}
			switch ev.Kind {
			case sip.EvRinging, sip.EvEarlyMedia:
				if !rang {
					rang = true
					o.Ringing()
				}
			case sip.EvAnswered:
				o.Answered(narrow(sc))
				a.answered(ctx)
			case sip.EvEnded:
				a.reason = reasonOf(ev)
				o.Release(a.reason)
				return
			}
		case ev, ok := <-o.Events():
			if !ok {
				sc.Hangup()
				return
			}
			switch ev.Kind {
			case core.CallReleased:
				a.reason = "normal"
				sc.Hangup()
				return
			default:
				a.handsetEvent(ctx, ev)
			}
		}
	}
}

// IncomingCall rings the handsets that have the account as a line. It is the
// sip.Endpoint, and returns at once.
func (m *Manager) IncomingCall(c *sip.Call) {
	go m.incoming(callInfo{ID: c.ID, AccountID: c.AccountID, Remote: c.Remote, RemoteDisplay: c.RemoteDisplay, RemoteShown: c.RemoteShown}, c)
}

// callInfo is what sip.Call has as fields.
type callInfo struct {
	ID            uuid.UUID
	AccountID     uuid.UUID
	Remote        string
	RemoteDisplay string
	RemoteShown   string // Remote as the line's number rules want it shown; "" means as it is
}

// legEv is an offer's event tagged with its leg.
type legEv struct {
	i  int
	ev core.CallEvent
}

func (m *Manager) incoming(ci callInfo, c SIPCall) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := m.log().With("sip_call", ci.ID)
	recv, err := m.Lines.Receivers(ctx, ci.AccountID)
	if err != nil || len(recv) == 0 {
		log.Info("a call to an account no handset has as a line", "account", ci.AccountID, "err", err)
		c.Reject(480)
		return
	}
	acct := ci.AccountID
	var hs *uuid.UUID
	if len(recv) == 1 {
		hs = idPtr(recv[0].Handset)
	}
	cl := m.logCall(ctx, calllog.Call{HandsetID: hs, SIPAccountID: &acct, Direction: "in", RemoteParty: ci.Remote})
	a := &active{m: m, sip: c, log: log, calllog: cl, reason: "missed"}
	for _, r := range recv {
		a.hids = append(a.hids, r.Handset)
	}
	defer a.finish(ctx)

	nb := narrow(c)
	fan := make(chan legEv, 64)
	done := make(chan struct{})
	defer close(done)
	var offers []Offer
	var legIdx []int // the receiver of each offer
	for i, r := range recv {
		o, err := m.DECT.Ring(ctx, r.Handset, core.Call{SIP: fmt.Sprintf("%s-%d", ci.ID, i), Line: r.Line,
			Number: cmp.Or(ci.RemoteShown, ci.Remote), Name: m.callerName(ctx, r.Handset, ci), Narrowband: nb})
		if err != nil {
			log.Info("a handset cannot be rung", "handset", r.Handset, "err", err)
			continue
		}
		n := len(offers)
		offers = append(offers, o)
		legIdx = append(legIdx, i)
		go func() {
			for ev := range o.Events() {
				select {
				case fan <- legEv{n, ev}:
				case <-done:
				}
			}
		}()
	}
	if len(offers) == 0 {
		c.Reject(480)
		return
	}
	over := make([]bool, len(offers))
	busy := 0
	cancelAll := func(except int) {
		for i, o := range offers {
			if i != except && !over[i] {
				o.Cancel()
			}
		}
	}
	defer func() { cancelAll(-1) }()
	winner, rang := -1, false
	for {
		select {
		case le := <-fan:
			ev := le.ev
			switch ev.Kind {
			case core.CallRinging:
				if !rang {
					rang = true
					_ = c.Ringing()
				}
			case core.CallAnswered:
				if winner >= 0 {
					break
				}
				winner = le.i
				cancelAll(winner)
				if err := c.Answer(); err != nil {
					log.Info("the call was answered on a handset, but the SIP call is gone", "err", err)
					return
				}
				log.Info("a call is answered", "handset", recv[legIdx[winner]].Handset, "cell", ev.Cell)
				a.reason = "normal"
				a.answered(ctx)
			case core.CallBusy, core.CallReleased:
				over[le.i] = true
				if ev.Kind == core.CallBusy {
					busy++
				}
				if le.i == winner {
					a.reason = "normal"
					c.Hangup()
					return
				}
				if winner < 0 && allTrue(over) {
					code := 480
					if busy == len(over) {
						code = 486
					}
					c.Reject(code)
					return
				}
			default:
				if le.i == winner {
					a.handsetEvent(ctx, ev)
				}
			}
		case ev, ok := <-c.Events():
			if !ok {
				return
			}
			if ev.Kind == sip.EvEnded {
				if winner >= 0 {
					a.reason = "normal"
				} else {
					a.reason = "missed"
				}
				return
			}
		}
	}
}

func allTrue(b []bool) bool {
	for _, x := range b {
		if !x {
			return false
		}
	}
	return true
}

func idPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// logCall opens the call's row, if there is a call log.
func (m *Manager) logCall(ctx context.Context, c calllog.Call) int64 {
	if m.CallLog == nil {
		return 0
	}
	id, err := m.CallLog.Start(ctx, c)
	if err != nil {
		m.log().Warn("the call could not be logged", "err", err)
		return 0
	}
	return id
}

// active is one call that is on: the SIP call, its speech once bound, and
// what the call log needs at the end. It is used by one goroutine.
type active struct {
	m       *Manager
	sip     SIPCall
	log     *slog.Logger
	calllog int64
	reason  string
	hids    []uuid.UUID // the handsets whose call lists this call is in

	sipAnswered bool
	answeredAt  time.Time
	pending     *core.CallEvent // the speech is bound, the media not open yet
	sess        *mediaSession
	opened      bool
}

// answered: the call is on, on both sides.
func (a *active) answered(ctx context.Context) {
	a.sipAnswered, a.answeredAt = true, time.Now()
	if a.calllog != 0 && a.m.CallLog != nil {
		_ = a.m.CallLog.Answered(ctx, a.calllog, a.answeredAt)
	}
	a.tryMedia(ctx)
}

// handsetEvent takes what the handset does during the call.
func (a *active) handsetEvent(ctx context.Context, ev core.CallEvent) {
	switch ev.Kind {
	case core.CallMediaUp:
		a.pending = &ev
		a.tryMedia(ctx)
	case core.CallMediaDown:
		a.pending = nil
		a.closeMedia()
	case core.CallDigit:
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := a.sip.SendDTMF(dctx, ev.Digit); err != nil && a.sess != nil {
			err = a.sess.pipe.SendDTMF(dctx, ev.Digit, 160*time.Millisecond)
			if err != nil {
				a.log.Info("a key could not be sent", "err", err)
			}
		}
	case core.CallHold, core.CallResume:
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := a.sip.Hold(hctx, ev.Kind == core.CallHold); err != nil {
			a.log.Info("hold could not be sent", "err", err)
		}
	}
}

// tryMedia opens the speech once the SIP side is answered and the chip's audio
// channel is bound, whichever came last.
func (a *active) tryMedia(ctx context.Context) {
	if !a.sipAnswered || a.pending == nil || a.sess != nil {
		return
	}
	ev := *a.pending
	a.pending = nil
	s, err := a.m.openMedia(ctx, ev, a.sip)
	if err != nil {
		a.log.Error("the call's speech could not be opened: the call is silent", "cell", ev.Cell, "err", err)
		return
	}
	a.sess, a.opened = s, true
}

func (a *active) closeMedia() {
	if a.sess != nil {
		a.m.closeMedia(a.sess)
	}
}

// finish records the end of the call and stops its speech. The quality
// summary is taken once the pipeline has stopped.
func (a *active) finish(ctx context.Context) {
	var q *calllogEnd
	if s := a.sess; s != nil {
		a.closeMedia()
		q = s.summary(a.sip)
	}
	if a.calllog == 0 || a.m.CallLog == nil {
		return
	}
	e := calllog.End{At: time.Now(), Reason: a.reason}
	if n := a.sip.Negotiated(); n != nil {
		e.SIPCodec = n.Codec.Name
		srtp := a.sip.Media().Secure()
		e.SRTP = &srtp
	}
	if q != nil {
		e.CellCodec, e.Quality = q.cellCodec, q.quality
	}
	// ctx may be cancelled by now: the row is written anyway
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.m.CallLog.Ended(wctx, a.calllog, e); err != nil {
		a.log.Warn("the end of the call could not be logged", "err", err)
	}
	if a.m.CallLists != nil {
		a.m.CallLists.CallListsChanged(a.hids...) // the handsets' lists have a new entry
	}
}

// listenMedia opens a UDP port in the media range for a cell's speech.
func (m *Manager) listenMedia() (*net.UDPConn, uint16, error) {
	lo, hi := m.MediaPorts[0], m.MediaPorts[1]
	try := func(port int) (*net.UDPConn, uint16, error) {
		c, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
		if err != nil {
			return nil, 0, err
		}
		return c, uint16(c.LocalAddr().(*net.UDPAddr).Port), nil
	}
	if lo <= 0 || hi < lo {
		return try(0)
	}
	n := hi - lo + 1
	start := int(time.Now().UnixNano() % int64(n))
	var last error
	for i := range n {
		c, p, err := try(lo + (start+i)%n)
		if err == nil {
			return c, p, nil
		}
		last = err
	}
	return nil, 0, fmt.Errorf("no free media port in %d-%d: %w", lo, hi, last)
}

// callerName is the name to show for a caller on one handset: the contact the handset knows the number under, else
// the name the other side sent.
func (m *Manager) callerName(ctx context.Context, handset uuid.UUID, ci callInfo) string {
	if m.Names != nil && ci.Remote != "" {
		if n, err := m.Names.LookupName(ctx, handset, ci.Remote); err == nil && n != "" {
			return n
		}
	}
	return ci.RemoteDisplay
}
