// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
)

// Endpoint is the DECT side of calls (C3). IncomingCall must return quickly;
// the endpoint later calls Ringing, Answer or Reject on the call.
type Endpoint interface {
	IncomingCall(c *Call)
}

// Config sets up a Manager.
type Config struct {
	// UDP is the bound UDP socket all accounts share (required).
	UDP net.PacketConn
	// TCP, if set, accepts incoming TCP connections. Outgoing TCP and TLS
	// connections are made as needed.
	TCP net.Listener
	// TLS configures outgoing TLS (root CAs; verification per account).
	TLS *tls.Config
	RTP *rtp.Ports
	// MediaIP, if valid, is advertised for RTP instead of the local
	// address towards the peer (a static public address).
	MediaIP   netip.Addr
	DNS       resolve.DNS
	Log       *slog.Logger
	UserAgent string
	Now       func() time.Time
}

// Manager runs every account's user agent over a shared transport set.
type Manager struct {
	cfg      Config
	log      *slog.Logger
	ua       *sipgo.UserAgent
	client   *sipgo.Client
	server   *sipgo.Server
	dialogs  *sipgo.DialogUA
	endpoint Endpoint
	udpPort  int
	tcpPort  int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	lines     map[uuid.UUID]*line
	byContact map[string]*line
	calls     map[string]*Call // by Call-ID
}

// New creates a manager; Start runs it.
func New(cfg Config, ep Endpoint) (*Manager, error) {
	if cfg.UDP == nil {
		return nil, errors.New("sip: a UDP socket is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.DNS == nil {
		cfg.DNS = &resolve.System{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "opendectcell-controller"
	}
	m := &Manager{cfg: cfg, log: cfg.Log, endpoint: ep, lines: map[uuid.UUID]*line{}, byContact: map[string]*line{}, calls: map[string]*Call{}}
	opts := []sipgo.UserAgentOption{sipgo.WithUserAgent(cfg.UserAgent)}
	if cfg.TLS != nil {
		opts = append(opts, sipgo.WithUserAgenTLSConfig(cfg.TLS))
	}
	ua, err := sipgo.NewUA(opts...)
	if err != nil {
		return nil, err
	}
	m.ua = ua
	// Send UDP from the shared listening socket: otherwise sipgo opens a
	// new ephemeral socket for the first request to each peer, and the
	// peer's later requests to our Contact would miss it.
	if m.client, err = sipgo.NewClient(ua, sipgo.WithClientLogger(cfg.Log),
		sipgo.WithClientConnectionAddr(cfg.UDP.LocalAddr().String())); err != nil {
		return nil, err
	}
	if m.server, err = sipgo.NewServer(ua, sipgo.WithServerLogger(cfg.Log)); err != nil {
		return nil, err
	}
	m.udpPort = cfg.UDP.LocalAddr().(*net.UDPAddr).Port
	// sipgo puts this Contact on requests it builds itself (ACK for a
	// 2xx); it must at least be well-formed. Our own requests carry the
	// account's Contact.
	m.dialogs = &sipgo.DialogUA{Client: m.client, ContactHDR: sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "opendectcell",
		Host: primaryIP().String(), Port: m.udpPort}}}
	if cfg.TCP != nil {
		m.tcpPort = cfg.TCP.Addr().(*net.TCPAddr).Port
	}
	m.server.OnInvite(m.onInvite)
	m.server.OnAck(m.onAck)
	m.server.OnBye(m.onBye)
	// A CANCEL that matches an INVITE is handled by the transaction layer
	// (200 to the CANCEL, 487 to the INVITE); only unmatched ones get here.
	m.server.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		m.respond(tx, req, 481, "Call/Transaction Does Not Exist")
	})
	m.server.OnOptions(m.onOptions)
	m.server.OnInfo(m.onInfo)
	m.server.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) { m.respond(tx, req, 200, "OK") }) // MWI: C10
	m.server.OnRefer(func(req *sip.Request, tx sip.ServerTransaction) { m.respond(tx, req, 501, "Not Implemented") })
	m.server.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) { m.respond(tx, req, 415, "Unsupported Media Type") })
	return m, nil
}

// Start serves the transports until ctx ends or Close is called.
func (m *Manager) Start(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.server.ServeUDP(m.cfg.UDP); err != nil && m.ctx.Err() == nil {
			m.log.Error("SIP UDP transport stopped", "err", err)
		}
	}()
	if m.cfg.TCP != nil {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			if err := m.server.ServeTCP(m.cfg.TCP); m.ctx.Err() == nil {
				m.log.Error("SIP TCP transport stopped", "err", err)
			}
		}()
	}
}

// Close unregisters every account (best effort, briefly), ends calls and
// stops the transports.
func (m *Manager) Close() error {
	m.mu.Lock()
	lines := make([]*line, 0, len(m.lines))
	for _, l := range m.lines {
		lines = append(lines, l)
	}
	calls := make([]*Call, 0, len(m.calls))
	for _, c := range m.calls {
		calls = append(calls, c)
	}
	m.mu.Unlock()
	for _, c := range calls {
		c.Hangup()
	}
	var wg sync.WaitGroup
	for _, l := range lines {
		wg.Add(1)
		go func(l *line) { defer wg.Done(); l.stop(true) }(l)
	}
	wg.Wait()
	if m.cancel != nil {
		m.cancel()
	}
	err := errors.Join(m.server.Close(), m.client.Close(), m.ua.Close())
	m.cfg.UDP.Close()
	if m.cfg.TCP != nil {
		m.cfg.TCP.Close()
	}
	m.wg.Wait()
	return err
}

// SetAccounts reconciles the running user agents with the given accounts:
// new ones start, removed ones unregister, changed ones restart. A changed
// account's new user agent starts only after the old one has unregistered:
// both use the same Contact, so a late unregister would remove the new
// binding.
func (m *Manager) SetAccounts(accts []Account) {
	want := map[uuid.UUID]Account{}
	for _, a := range accts {
		if err := a.Validate(); err != nil {
			m.log.Warn("SIP account skipped", "account", a.ID, "err", err)
			continue
		}
		want[a.ID] = a
	}
	m.mu.Lock()
	old := map[uuid.UUID]*line{}
	for id, l := range m.lines {
		if a, ok := want[id]; !ok || !l.same(a) {
			old[id] = l
			delete(m.lines, id)
			delete(m.byContact, ContactUser(id))
		}
	}
	var start []*line
	for id, a := range want {
		if _, ok := m.lines[id]; ok {
			continue
		}
		l := newLine(m, a)
		m.lines[id] = l
		m.byContact[ContactUser(id)] = l
		start = append(start, l)
	}
	m.mu.Unlock()
	for _, l := range start {
		if prev := old[l.acct.ID]; prev != nil {
			delete(old, l.acct.ID)
			go func(prev, l *line) { prev.stop(true); l.start() }(prev, l)
			continue
		}
		l.start()
	}
	for _, l := range old {
		go l.stop(true)
	}
}

// Status returns an account's registration state.
func (m *Manager) Status(id uuid.UUID) (RegStatus, bool) {
	m.mu.Lock()
	l := m.lines[id]
	m.mu.Unlock()
	if l == nil {
		return RegStatus{}, false
	}
	return l.status(), true
}

func (m *Manager) line(id uuid.UUID) *line {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lines[id]
}

// transport helpers

func sipTransport(t resolve.Transport) string { return strings.ToUpper(string(t)) }

// primaryIP is the address of the default route, or loopback.
func primaryIP() netip.Addr {
	if a := localAddrFor(netip.MustParseAddrPort("192.0.2.1:5060")); a.IsValid() {
		return a
	}
	return netip.MustParseAddr("127.0.0.1")
}

// localAddrFor returns the local IP the kernel would use towards a target.
// No packet is sent.
func localAddrFor(target netip.AddrPort) netip.Addr {
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(target))
	if err != nil {
		return netip.Addr{}
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
}

// via builds our Via for a request to target. For UDP, sent-by is the
// shared socket's port; for TCP/TLS the transport fills in the connection's
// port.
func (m *Manager) via(target resolve.Target) *sip.ViaHeader {
	v := &sip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: sipTransport(target.Transport),
		Host: localAddrFor(target.Addr).String(), Params: sip.NewParams()}
	if target.Transport == resolve.UDP {
		v.Port = m.udpPort
	}
	v.Params.Add("branch", sip.GenerateBranch())
	v.Params.Add("rport", "")
	return v
}

// contactPort is the port to advertise in Contact for a transport.
func (m *Manager) contactPort(t resolve.Transport) int {
	if t == resolve.UDP || m.tcpPort == 0 {
		return m.udpPort
	}
	return m.tcpPort
}

func (m *Manager) respond(tx sip.ServerTransaction, req *sip.Request, code int, reason string, hdrs ...sip.Header) {
	res := sip.NewResponseFromRequest(req, code, reason, nil)
	for _, h := range hdrs {
		res.AppendHeader(h)
	}
	if err := tx.Respond(res); err != nil {
		m.log.Debug("SIP respond failed", "code", code, "err", err)
	}
}

// lineFor finds the account a request is for: by our Contact user part in
// the Request-URI, else by the AOR user in To (some PBXes address the AOR).
func (m *Manager) lineFor(req *sip.Request) *line {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.byContact[req.Recipient.User]; l != nil {
		return l
	}
	if to := req.To(); to != nil {
		var found *line
		for _, l := range m.lines {
			if l.acct.Username == to.Address.User && strings.EqualFold(l.acct.Domain, to.Address.Host) {
				if found != nil {
					return nil // ambiguous
				}
				found = l
			}
		}
		return found
	}
	return nil
}

func (m *Manager) onOptions(req *sip.Request, tx sip.ServerTransaction) {
	m.respond(tx, req, 200, "OK", sip.NewHeader("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS, INFO, NOTIFY"),
		sip.NewHeader("Accept", "application/sdp"))
}

func (m *Manager) callByID(req *sip.Request) *Call {
	id := req.CallID()
	if id == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[id.Value()]
}

func (m *Manager) addCall(c *Call) {
	m.mu.Lock()
	m.calls[c.callID] = c
	m.mu.Unlock()
}

func (m *Manager) removeCall(c *Call) {
	m.mu.Lock()
	if m.calls[c.callID] == c {
		delete(m.calls, c.callID)
	}
	m.mu.Unlock()
}

func (m *Manager) onAck(req *sip.Request, tx sip.ServerTransaction) {
	if c := m.callByID(req); c != nil {
		c.readAck(req, tx)
	}
}

func (m *Manager) onBye(req *sip.Request, tx sip.ServerTransaction) {
	c := m.callByID(req)
	if c == nil {
		m.respond(tx, req, 481, "Call/Transaction Does Not Exist")
		return
	}
	c.readBye(req, tx)
}

func (m *Manager) onInfo(req *sip.Request, tx sip.ServerTransaction) {
	c := m.callByID(req)
	if c == nil {
		m.respond(tx, req, 481, "Call/Transaction Does Not Exist")
		return
	}
	c.readInfo(req, tx)
}

func (m *Manager) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	if to := req.To(); to != nil {
		if _, ok := to.Params.Get("tag"); ok {
			// In-dialog: a re-INVITE (hold, resume, media change).
			c := m.callByID(req)
			if c == nil {
				m.respond(tx, req, 481, "Call/Transaction Does Not Exist")
				return
			}
			c.readReinvite(req, tx)
			return
		}
	}
	l := m.lineFor(req)
	if l == nil {
		m.respond(tx, req, 404, "Not Found")
		return
	}
	m.respond(tx, req, 100, "Trying")
	l.incoming(req, tx)
}

// buildRequest is the ClientRequestOption for requests we build: sipgo's
// defaults for missing headers, and the shared UDP socket only for UDP
// (a TCP or TLS connection must not be bound to the UDP port).
func buildRequest(c *sipgo.Client, r *sip.Request) error {
	if err := sipgo.ClientRequestBuild(c, r); err != nil {
		return err
	}
	if r.Transport() != "UDP" {
		r.Laddr = sip.Addr{}
	}
	return nil
}

// errorf is fmt.Errorf with the package prefix.
func errorf(format string, a ...any) error { return fmt.Errorf("sip: "+format, a...) }
