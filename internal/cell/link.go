// SPDX-License-Identifier: AGPL-3.0-or-later

package cell

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/pki"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
)

// Timing defaults (§6.3, §6.4).
const (
	DefaultHeartbeat = 1000 * time.Millisecond
	DefaultDead      = 3000 * time.Millisecond
	DefaultGrace     = 7000 * time.Millisecond
	DefaultOrphan    = 60 * time.Second

	helloTimeout     = 5 * time.Second
	handshakeTimeout = 10 * time.Second
	enrollTimeout    = 10 * time.Second
	enrollDelay      = 30 * time.Second
	writeTimeout     = 5 * time.Second
	rttWindow        = 60
)

// controllerCaps are the capabilities the controller uses (§6.1). MAIL is
// required in v1: the tunnel ends at Server.Mails; without one, mails are
// counted and dropped. MEDIA and SRTP: the controller sends MEDIA_OPEN
// (bridge.Manager); a cell refuses it ("not allowed") unless HELLO_ACK
// grants both. Plain RTP is never granted here: it needs the cell's
// allow_plain_rtp and is not used.
const controllerCaps = wire.CapMail | wire.CapMedia | wire.CapSRTP | wire.CapStats | wire.CapFlow | wire.CapResume

// Server accepts cell connections on the control port.
type Server struct {
	Store *Store
	Log   *slog.Logger
	Hosts []string // extra DNS/IP SANs for the server certificate

	Heartbeat, Dead, Grace, Orphan time.Duration
	// EnrollDelay is how long a rate-limited enrollment waits before its
	// answer (§5.2); tests shorten it.
	EnrollDelay time.Duration
	// Mails receives the STUB mail tunnel of every active cell (§7); nil
	// drops the mails. Set before Serve.
	Mails MailSink
	// Media is told of the cell's answers about media flows (§8.1); nil
	// ignores them. Set before Serve.
	Media MediaSink

	mu       sync.Mutex
	cells    map[uuid.UUID]*record
	cert     *tls.Certificate
	certExp  time.Time
	tlsConf  *tls.Config
	sessions sync.WaitGroup
}

// MailSink is the far end of the STUB mail tunnel (§7), the DECT core.
// Its methods run on the cell's session goroutine, in the order the cell
// sent: they must not block. Param slices are only valid during the call.
// Never log the Param of the primitives in §7.3.
type MailSink interface {
	MailUp(cell uuid.UUID, m wire.MailUpMsg)
	MailReject(cell uuid.UUID, m wire.MailRejectMsg)
}

// MediaSink is told what the cell says about a media flow (§8.1): its answer
// to MEDIA_OPEN and a flow that ended. The methods run on the cell's session
// goroutine and must not block or call back into the Server.
type MediaSink interface {
	MediaOpened(cell uuid.UUID, a wire.MediaOpenAckMsg)
	MediaClosed(cell uuid.UUID, c wire.MediaClosedMsg)
}

// StatusWatcher is also told of the cell's STATUS (radio and chip state), if
// Server.Mails implements it. Same rules as CellWatcher.
type StatusWatcher interface {
	// CellRadio: the cell reported its radio (0 off, 1 on, 2 fault) and its
	// chip session (1 down, 2 up).
	CellRadio(cell uuid.UUID, radio, chip uint8)
}

// CellWatcher is told when an active cell's mail tunnel opens and when its
// DECT state ends (§6.4). Server.Mails gets these calls if it implements
// it. The methods must not block or call back into the Server.
type CellWatcher interface {
	// CellUp: an active cell's session started, or resumed (§6.4).
	CellUp(cell uuid.UUID, up CellUp)
	// CellDown: the link is lost; the grace period runs and the cell's
	// state is kept.
	CellDown(cell uuid.UUID)
	// CellGone: the grace period ended, or the cell is no longer active:
	// its calls and DECT state end.
	CellGone(cell uuid.UUID)
}

// CellUp is what the DECT core needs from a cell's HELLO (§6.1).
type CellUp struct {
	Resumed       bool  // the previous session resumed: keep its state
	RadioState    uint8 // tag 14: 0 off, 1 on, 2 fault
	CapSubRecords uint8
	StubVersion   string // tag 3: the chip firmware build, "06.88" etc.
	HWModel       string
	// RFPI is what the cell's chip broadcasts (tag 17); HasRFPI is false for
	// a cell that does not report it (older software).
	RFPI    [5]byte
	HasRFPI bool
}

func (s *Server) watcher() CellWatcher {
	w, _ := s.Mails.(CellWatcher)
	return w
}

// ErrNoCell means the cell has no active session to carry a mail.
var ErrNoCell = errors.New("cell: not connected or not active")

// SendMail queues a MAIL_DOWN for an active cell (§7.1). corr 0 means the
// cell drops it silently if the chip cannot take it; any other corr comes
// back in a MAIL_REJECT (§7.2). Mails of one caller keep their order.
// SetIdentity asks an active cell to broadcast rfpi (CONFIG_SET tag 6), or, with
// nil, to go back to its own identity. The cell reloads its chip if it differs
// from what it broadcasts now, and reconnects.
func (s *Server) SetIdentity(id uuid.UUID, rfpi *[5]byte) error {
	s.mu.Lock()
	var ss *session
	if rec := s.cells[id]; rec != nil {
		ss = rec.sess
	}
	s.mu.Unlock()
	if ss == nil || !ss.active.Load() {
		return ErrNoCell
	}
	var m wire.ConfigSetMsg
	if rfpi != nil {
		m = wire.ConfigSetMsg{RFPI: *rfpi, HasRFPI: true}
	} else {
		m = wire.ConfigSetMsg{Revert: true}
	}
	ss.send(wire.Frame{Type: wire.ConfigSet, Corr: 1, Body: m.Encode()})
	return nil
}

func (s *Server) SendMail(id uuid.UUID, corr uint16, m wire.MailDownMsg) error {
	s.mu.Lock()
	var ss *session
	if rec := s.cells[id]; rec != nil {
		ss = rec.sess
	}
	s.mu.Unlock()
	if ss == nil || !ss.active.Load() {
		return ErrNoCell
	}
	ss.send(wire.Frame{Type: wire.MailDown, Corr: corr, Body: m.Encode()})
	s.updateStatus(id, func(st *Status) { st.MailsDown++ })
	return nil
}

// OpenMedia asks an active cell to tie a chip channel to a UDP flow
// (MEDIA_OPEN, §8.1). The answer comes to Server.Media.
func (s *Server) OpenMedia(id uuid.UUID, m wire.MediaOpenMsg) error {
	// The cell echoes corr in an ERROR it answers with, which is how a refusal
	// finds its call (the media id, which is small, as a 16 bit number).
	return s.sendFrame(id, wire.Frame{Type: wire.MediaOpen, Corr: uint16(m.MediaID), Body: m.Encode()})
}

// CloseMedia asks the cell to end a flow (MEDIA_CLOSE); it answers
// MEDIA_CLOSED.
func (s *Server) CloseMedia(id uuid.UUID, mediaID uint32) error {
	return s.sendFrame(id, wire.Frame{Type: wire.MediaClose, Body: wire.EncodeMediaClose(mediaID)})
}

func (s *Server) sendFrame(id uuid.UUID, f wire.Frame) error {
	s.mu.Lock()
	var ss *session
	if rec := s.cells[id]; rec != nil {
		ss = rec.sess
	}
	s.mu.Unlock()
	if ss == nil || !ss.active.Load() {
		return ErrNoCell
	}
	ss.send(f)
	return nil
}

// record is the controller's per-cell state across connections: the live
// session, if any, and the resume and orphan timers.
type record struct {
	sess         *session
	sessionToken [16]byte
	bootID       [16]byte
	chipEpoch    uint32
	seenHello    bool
	disconnected time.Time
	graceTimer   *time.Timer
	orphanTimer  *time.Timer
	offline      bool
	status       Status
}

// Status is a cell's live state for the UI.
type Status struct {
	Online      bool
	Since       time.Time
	RemoteAddr  string
	SWVersion   string
	RadioState  uint8
	ChipState   uint8
	RFPI        [5]byte // what the chip broadcasts, if the cell said (HasRFPI)
	HasRFPI     bool
	ChipEpoch   uint32
	UptimeS     uint32
	Detail      string
	RTTp50      time.Duration
	RTTp95      time.Duration
	Stats       map[string]uint64
	StatsAt     time.Time
	FlowState   uint8
	MailsUp     uint64 // MAIL_UP handed to Server.Mails
	MailsDown   uint64 // MAIL_DOWN queued by SendMail
	MailsReject uint64 // MAIL_REJECT from the cell
	MailsDrop   uint64 // MAIL_UP with no Server.Mails to take it
	Resumes     int
	Supersedes  int
	LastGoodbye string
	// Audio as announced in HELLO (cell-protocol.md §8.2), for the media
	// engine: formats accepted (downlink) and sent (uplink), preferred
	// first, and the host audio endpoints per bandwidth.
	AudioDown, AudioUp     []uint8
	CapAudioNB, CapAudioWB uint8
}

// Health classifies the RTT as in §2 rule 5.
func (st Status) Health() string {
	switch {
	case !st.Online:
		return "offline"
	case st.RTTp95 > 400*time.Millisecond:
		return "unfit"
	case st.RTTp95 > 150*time.Millisecond:
		return "degraded"
	}
	return "ok"
}

func (s *Server) defaults() {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	for p, d := range map[*time.Duration]time.Duration{&s.Heartbeat: DefaultHeartbeat, &s.Dead: DefaultDead, &s.Grace: DefaultGrace, &s.Orphan: DefaultOrphan, &s.EnrollDelay: enrollDelay} {
		if *p == 0 {
			*p = d
		}
	}
}

func (s *Server) serverCert(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Store.now()
	// Reissue well before expiry; cells pin the CA, not the leaf.
	if s.cert == nil || now.After(s.certExp.Add(-30*24*time.Hour)) {
		der, key, err := s.Store.CA.ServerCert(s.Store.Installation, s.Hosts, now)
		if err != nil {
			return nil, err
		}
		leaf, _ := x509.ParseCertificate(der)
		s.cert = &tls.Certificate{Certificate: [][]byte{der, s.Store.CA.Cert.Raw}, PrivateKey: key, Leaf: leaf}
		s.certExp = leaf.NotAfter
	}
	return s.cert, nil
}

// TLSConfig is the listener's configuration (§4): TLS 1.3 only; a client
// certificate is verified against the cell CA if given, and connections
// without one may only enroll.
func (s *Server) TLSConfig() *tls.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tlsConf == nil {
		pool := x509.NewCertPool()
		pool.AddCert(s.Store.CA.Cert)
		s.tlsConf = &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: s.serverCert,
			ClientAuth:     tls.VerifyClientCertIfGiven,
			ClientCAs:      pool,
		}
	}
	return s.tlsConf
}

// Serve accepts connections on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.defaults()
	s.mu.Lock()
	if s.cells == nil {
		s.cells = map[uuid.UUID]*record{}
	}
	s.mu.Unlock()
	tl := tls.NewListener(ln, s.TLSConfig())
	go func() { <-ctx.Done(); tl.Close() }()
	for {
		c, err := tl.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.sessions.Wait()
				return nil //nolint:nilerr // Accept fails because we closed the listener on shutdown
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return err
		}
		s.sessions.Add(1)
		go func() {
			defer s.sessions.Done()
			s.handle(ctx, c.(*tls.Conn))
		}()
	}
}

func remoteAddr(c net.Conn) netip.Addr {
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

func (s *Server) handle(ctx context.Context, c *tls.Conn) {
	defer c.Close()
	addr := remoteAddr(c)
	ctx = audit.WithActor(ctx, audit.Actor{Name: "cell-link", Addr: addr})
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := c.HandshakeContext(ctx); err != nil {
		s.Log.Debug("cell TLS handshake failed", "addr", addr, "err", err)
		return
	}
	tuneSocket(c.NetConn(), s.Log)
	peers := c.ConnectionState().PeerCertificates
	if len(peers) == 0 {
		s.enroll(ctx, c, addr)
		return
	}
	info, err := s.Store.Authenticate(ctx, peers[0])
	switch {
	case errors.Is(err, ErrRevoked):
		s.Log.Info("revoked cell tried to connect", "cell", info.ID, "addr", addr)
		_ = writeFrame(c, wire.Frame{Type: wire.Goodbye, Body: wire.GoodbyeMsg{Reason: wire.ByeRevoked}.Encode()})
		return
	case err != nil:
		s.Log.Info("cell certificate not accepted", "addr", addr, "err", err)
		_ = writeFrame(c, wire.Frame{Type: wire.Goodbye, Body: wire.GoodbyeMsg{Reason: wire.ByeAuth}.Encode()})
		return
	}
	s.runSession(ctx, c, info, peers[0], addr)
}

func writeFrame(w io.Writer, f wire.Frame) error {
	b, err := f.Append(nil)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// enroll serves one ENROLL_REQUEST on a connection without a client
// certificate (§5.2). Anything else closes the connection without reply.
func (s *Server) enroll(ctx context.Context, c *tls.Conn, addr netip.Addr) {
	c.SetDeadline(time.Now().Add(enrollTimeout))
	f, err := wire.ReadFrame(bufio.NewReader(c))
	if err != nil || f.Type != wire.EnrollRequest || f.CheckBound() != nil {
		return
	}
	resp := wire.EnrollResponseMsg{Status: wire.EnrollInternal}
	req, err := wire.DecodeEnrollRequest(f.Body)
	if err != nil {
		resp.Status = wire.EnrollBadCSR
		if len(req.Token) != wire.TokenLen {
			resp.Status = wire.EnrollTokenRejected
		}
	} else {
		res, err := s.Store.Enroll(ctx, req, addr.String())
		switch {
		case err == nil:
			resp = wire.EnrollResponseMsg{Status: wire.EnrollOK, CellID: res.CellID, Cert: res.Cert.Raw, CACert: s.Store.CA.Cert.Raw, State: res.State}
			s.Log.Info("cell enrolled", "cell", res.CellID, "state", res.State, "addr", addr)
		case errors.Is(err, ErrTokenRejected):
			resp.Status = wire.EnrollTokenRejected
		case errors.Is(err, pki.ErrBadCSR):
			resp.Status = wire.EnrollBadCSR
		case errors.Is(err, ErrRateLimited):
			resp.Status = wire.EnrollRateLimited
			select {
			case <-time.After(s.EnrollDelay):
			case <-ctx.Done():
				return
			}
		default:
			s.Log.Error("enrollment failed", "err", err)
		}
	}
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	_ = writeFrame(c, wire.Frame{Type: wire.EnrollResponse, Corr: f.Corr, Body: resp.Encode()})
}

// session is one authenticated connection.
type session struct {
	srv    *Server
	conn   *tls.Conn
	cell   *Info
	cert   *x509.Certificate
	hello  wire.HelloMsg
	addr   netip.Addr
	log    *slog.Logger
	queues [3]chan wire.Frame
	ctrl   chan func()
	done   chan struct{}
	close  sync.Once
	rtts   []time.Duration
	hbSent map[uint32]time.Time
	start  time.Time
	// lastWrite is when the writer last sent a frame (UnixNano), for the
	// heartbeat-when-idle rule (§6.3).
	lastWrite atomic.Int64
	// active mirrors cell.State == StateActive for SendMail, which runs
	// on other goroutines.
	active atomic.Bool
}

// errClosed ends a session without an error to report.
var errClosed = errors.New("session closed")

func (s *Server) runSession(ctx context.Context, c *tls.Conn, info *Info, cert *x509.Certificate, addr netip.Addr) {
	ss := &session{srv: s, conn: c, cell: info, cert: cert, addr: addr,
		log:  s.Log.With("cell", info.ID, "addr", addr),
		ctrl: make(chan func(), 8), done: make(chan struct{}), hbSent: map[uint32]time.Time{}, start: time.Now()}
	ss.active.Store(info.State == wire.StateActive)
	for i := range ss.queues {
		ss.queues[i] = make(chan wire.Frame, []int{64, 32, 64}[i])
	}
	r := bufio.NewReader(c)

	// HELLO within 5 s, or close (§6.1).
	c.SetDeadline(time.Now().Add(helloTimeout))
	f, err := wire.ReadFrame(r)
	if err != nil || f.Type != wire.Hello {
		ss.log.Info("no HELLO from cell", "err", err)
		return
	}
	hello, err := wire.DecodeHello(f.Body)
	if hello.Major != wire.ProtoMajor && (err == nil || hello.Major != 0) {
		_ = writeFrame(c, wire.Frame{Type: wire.Goodbye, Body: wire.GoodbyeMsg{Reason: wire.ByeVersion,
			Text: fmt.Sprintf("controller speaks %d.%d", wire.ProtoMajor, wire.ProtoMinor)}.Encode()})
		ss.log.Info("cell protocol version not supported", "major", hello.Major)
		return
	}
	if err != nil {
		_ = writeFrame(c, wire.Frame{Type: wire.Error, Corr: f.Corr, Body: wire.ErrorMsg{Code: wire.ErrMalformedC, RefType: wire.Hello, Text: err.Error()}.Encode()})
		_ = writeFrame(c, wire.Frame{Type: wire.Goodbye, Body: wire.GoodbyeMsg{Reason: wire.ByeProtocol}.Encode()})
		return
	}
	c.SetDeadline(time.Time{})
	ss.hello = hello

	resumed, tok, err := s.attach(ss, hello)
	if err != nil {
		ss.log.Error("attach session", "err", err)
		return
	}
	defer s.detach(ss)
	if err := s.Store.RecordHello(ctx, info.ID, hello); err != nil {
		ss.log.Warn("record HELLO", "err", err)
	}
	if err := s.Store.RetireOldCerts(ctx, info.ID, cert); err != nil {
		ss.log.Warn("retire old certificates", "err", err)
	}
	ack := wire.HelloAckMsg{Major: wire.ProtoMajor, Minor: wire.ProtoMinor, CellID: info.ID, State: info.State,
		SessionToken: tok, Resumed: resumed, HeartbeatMs: uint16(s.Heartbeat / time.Millisecond),
		DeadMs: uint16(s.Dead / time.Millisecond), GraceMs: uint16(s.Grace / time.Millisecond),
		OrphanS: uint16(s.Orphan / time.Second), Capabilities: hello.Capabilities & controllerCaps,
		ControllerTimeMs: uint64(time.Now().UnixMilli())}
	ss.log.Info("cell connected", "resumed", resumed, "state", info.State, "sw", hello.SWVersion)

	writerDone := make(chan error, 1)
	go func() { writerDone <- ss.writer() }()
	ss.send(wire.Frame{Type: wire.HelloAck, Corr: f.Corr, Body: ack.Encode()})
	ss.send(wire.Frame{Type: wire.CellState, Body: wire.CellStateMsg{State: info.State}.Encode()})
	if w := s.watcher(); w != nil && info.State == wire.StateActive {
		w.CellUp(info.ID, ss.cellUp(resumed))
	}

	frames := make(chan wire.Frame, 16)
	readErr := make(chan error, 1)
	go func() {
		for {
			f, err := wire.ReadFrame(r)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- f:
			case <-ss.done:
				return
			}
		}
	}()

	hb := time.NewTicker(s.Heartbeat)
	defer hb.Stop()
	dead := time.NewTimer(s.Dead)
	defer dead.Stop()
	var reason error
loop:
	for {
		select {
		case f := <-frames:
			dead.Reset(s.Dead)
			if err := ss.handleFrame(ctx, f); err != nil {
				reason = err
				break loop
			}
		case err := <-readErr:
			if errors.Is(err, wire.ErrFraming) {
				ss.sendNow(wire.Frame{Type: wire.Goodbye, Body: wire.GoodbyeMsg{Reason: wire.ByeProtocol}.Encode()})
			}
			reason = err
			break loop
		case <-dead.C:
			reason = errors.New("link dead: nothing received for dead_ms")
			break loop
		case <-hb.C:
			// Send a heartbeat only if nothing else went out for
			// heartbeat_ms (§6.3). A tick can come slightly early, hence
			// the 10 % slack.
			if time.Since(time.Unix(0, ss.lastWrite.Load())) >= s.Heartbeat*9/10 {
				t := uint32(time.Since(ss.start) / time.Millisecond)
				ss.hbSent[t] = time.Now()
				ss.send(wire.Frame{Type: wire.Heartbeat, Body: wire.HeartbeatBody(t)})
			}
			if len(ss.hbSent) > 16 { // unanswered heartbeats: keep the map small
				for k := range ss.hbSent {
					delete(ss.hbSent, k)
				}
			}
		case fn := <-ss.ctrl:
			fn()
		case err := <-writerDone:
			reason = err
			break loop
		case <-ss.done:
			reason = errClosed
			break loop
		case <-ctx.Done():
			ss.sendNow(wire.Frame{Type: wire.Goodbye, Body: wire.GoodbyeMsg{Reason: wire.ByeShutdown}.Encode()})
			reason = ctx.Err()
			break loop
		}
	}
	ss.shutdown()
	if reason != nil && !errors.Is(reason, errClosed) && !errors.Is(reason, io.EOF) && !errors.Is(reason, context.Canceled) {
		ss.log.Info("cell link closed", "reason", reason)
	} else {
		ss.log.Info("cell link closed")
	}
}

func (ss *session) cellUp(resumed bool) CellUp {
	h := ss.hello
	return CellUp{Resumed: resumed, RadioState: h.RadioState, CapSubRecords: h.CapSubRecords,
		StubVersion: h.StubVersion, HWModel: h.HWModel, RFPI: h.RFPI, HasRFPI: h.HasRFPI}
}

func (ss *session) shutdown() {
	ss.close.Do(func() {
		close(ss.done)
		ss.conn.Close()
	})
}

// send queues a frame by its class (§9.1). A full P0 or P1 queue means the
// peer is not reading: the link is closed with OVERLOAD (§9.2). A full P2
// queue drops the frame.
func (ss *session) send(f wire.Frame) {
	info, _ := wire.Lookup(f.Type)
	select {
	case ss.queues[info.Class] <- f:
	default:
		if info.Class == wire.P2 {
			return
		}
		ss.log.Warn("send queue full; closing", "class", info.Class)
		ss.sendNow(wire.Frame{Type: wire.Goodbye, Body: wire.GoodbyeMsg{Reason: wire.ByeOverload}.Encode()})
		ss.shutdown()
	}
}

// sendNow writes directly, bypassing the queues, for a final GOODBYE.
func (ss *session) sendNow(f wire.Frame) {
	ss.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = writeFrame(ss.conn, f)
}

// writer drains the queues with strict priority: P0, then P1, then P2.
func (ss *session) writer() error {
	var buf []byte
	write := func(f wire.Frame) error {
		var err error
		buf, err = f.Append(buf[:0])
		if err != nil {
			return err
		}
		ss.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		_, err = ss.conn.Write(buf)
		ss.lastWrite.Store(time.Now().UnixNano())
		return err
	}
	for {
		select {
		case f := <-ss.queues[wire.P0]:
			if err := write(f); err != nil {
				return err
			}
			continue
		default:
		}
		select {
		case f := <-ss.queues[wire.P0]:
			if err := write(f); err != nil {
				return err
			}
			continue
		case f := <-ss.queues[wire.P1]:
			if err := write(f); err != nil {
				return err
			}
			continue
		default:
		}
		select {
		case f := <-ss.queues[wire.P0]:
			if err := write(f); err != nil {
				return err
			}
		case f := <-ss.queues[wire.P1]:
			if err := write(f); err != nil {
				return err
			}
		case f := <-ss.queues[wire.P2]:
			if err := write(f); err != nil {
				return err
			}
		case <-ss.done:
			return errClosed
		}
	}
}

func (ss *session) errorReply(f wire.Frame, code uint16, text string) {
	ss.send(wire.Frame{Type: wire.Error, Corr: f.Corr, Body: wire.ErrorMsg{Code: code, RefType: f.Type, RefCorr: f.Corr, Text: text}.Encode()})
}

// handleFrame processes one frame from the cell. A returned error ends the
// session.
func (ss *session) handleFrame(ctx context.Context, f wire.Frame) error {
	info, known := wire.Lookup(f.Type)
	if !known {
		ss.log.Warn("unknown frame type from cell", "type", f.Type)
		ss.errorReply(f, wire.ErrUnknownType, "")
		return nil
	}
	if len(f.Body) > info.MaxBody {
		ss.errorReply(f, wire.ErrTooLarge, "")
		return nil
	}
	malformed := func(err error) error {
		ss.errorReply(f, wire.ErrMalformedC, err.Error())
		return nil
	}
	switch f.Type {
	case wire.Heartbeat:
		if _, err := wire.DecodeHeartbeat(f.Body); err != nil {
			return malformed(err)
		}
		ss.send(wire.Frame{Type: wire.HeartbeatEcho, Corr: f.Corr, Body: f.Body})
	case wire.HeartbeatEcho:
		t, err := wire.DecodeHeartbeat(f.Body)
		if err != nil {
			return malformed(err)
		}
		if sent, ok := ss.hbSent[t]; ok {
			delete(ss.hbSent, t)
			ss.addRTT(time.Since(sent))
		}
	case wire.Goodbye:
		g, _ := wire.DecodeGoodbye(f.Body)
		ss.srv.updateStatus(ss.cell.ID, func(st *Status) { st.LastGoodbye = fmt.Sprintf("%d %s", g.Reason, g.Text) })
		return errClosed
	case wire.Error:
		e, _ := wire.DecodeError(f.Body)
		ss.log.Warn("ERROR from cell", "code", e.Code, "ref_type", e.RefType, "text", e.Text)
		if e.RefType == wire.MediaOpen && ss.srv.Media != nil {
			// a refused MEDIA_OPEN is an answer: the call need not wait for one
			ss.srv.Media.MediaOpened(ss.cell.ID, wire.MediaOpenAckMsg{MediaID: uint32(e.RefCorr), Status: wire.MediaInternalError})
		}
	case wire.Status:
		m, err := wire.DecodeStatus(f.Body)
		if err != nil {
			return malformed(err)
		}
		ss.srv.updateStatus(ss.cell.ID, func(st *Status) {
			if m.HasRadio {
				st.RadioState = m.RadioState
			}
			if m.HasChip {
				st.ChipState = m.ChipState
			}
			st.ChipEpoch, st.UptimeS, st.Detail = m.ChipEpoch, m.UptimeS, m.Detail
			if m.HasRFPI {
				st.RFPI, st.HasRFPI = m.RFPI, true
			}
		})
		if w, ok := ss.srv.Mails.(StatusWatcher); ok && m.HasRadio && m.HasChip {
			w.CellRadio(ss.cell.ID, m.RadioState, m.ChipState)
		}
	case wire.Stats:
		c, err := wire.DecodeStats(f.Body)
		if err != nil {
			return malformed(err)
		}
		named := map[string]uint64{}
		for id, v := range c {
			n, ok := wire.StatsCounterNames[id]
			if !ok {
				n = fmt.Sprintf("counter_%d", id)
			}
			named[n] = v
		}
		ss.srv.updateStatus(ss.cell.ID, func(st *Status) { st.Stats, st.StatsAt = named, time.Now() })
		if err := ss.srv.Store.Seen(ctx, ss.cell.ID); err != nil {
			ss.log.Warn("record last seen", "err", err)
		}
	case wire.Flow:
		m, err := wire.DecodeFlow(f.Body)
		if err != nil {
			return malformed(err)
		}
		ss.srv.updateStatus(ss.cell.ID, func(st *Status) { st.FlowState = m.State })
	case wire.StateSnapshot:
		// No DECT sessions or media exist yet (C3, C5), so there is nothing
		// to reconcile; accept it.
		if _, err := wire.ParseTLV(f.Body); err != nil {
			return malformed(err)
		}
	case wire.RenewRequest:
		csr, err := wire.DecodeRenewRequest(f.Body)
		if err != nil {
			return malformed(err)
		}
		cert, err := ss.srv.Store.Renew(ctx, ss.cell.ID, csr)
		switch {
		case err == nil:
			ss.send(wire.Frame{Type: wire.RenewResponse, Corr: f.Corr, Body: wire.EncodeRenewResponse(0, cert.Raw)})
		case errors.Is(err, pki.ErrBadCSR):
			ss.send(wire.Frame{Type: wire.RenewResponse, Corr: f.Corr, Body: wire.EncodeRenewResponse(2, nil)})
		default:
			ss.log.Error("certificate renewal", "err", err)
			ss.send(wire.Frame{Type: wire.RenewResponse, Corr: f.Corr, Body: wire.EncodeRenewResponse(4, nil)})
		}
	case wire.MailUp, wire.MailReject:
		if ss.cell.State != wire.StateActive {
			ss.errorReply(f, wire.ErrNotAllowed, "cell is not active")
			return nil
		}
		sink := ss.srv.Mails
		if f.Type == wire.MailReject {
			m, err := wire.DecodeMailReject(f.Body)
			if err != nil {
				return malformed(err)
			}
			ss.srv.updateStatus(ss.cell.ID, func(st *Status) { st.MailsReject++ })
			if sink != nil {
				sink.MailReject(ss.cell.ID, m)
			}
			return nil
		}
		m, err := wire.DecodeMailUp(f.Body)
		if err != nil {
			return malformed(err)
		}
		if sink == nil {
			ss.srv.updateStatus(ss.cell.ID, func(st *Status) { st.MailsDrop++ })
			return nil
		}
		ss.srv.updateStatus(ss.cell.ID, func(st *Status) { st.MailsUp++ })
		sink.MailUp(ss.cell.ID, m)
	case wire.ConfigAck:
		st, err := wire.DecodeConfigAck(f.Body)
		if err != nil {
			return malformed(err)
		}
		ss.log.Info("cell answered CONFIG_SET", "status", st)
	case wire.MediaOpenAck:
		if ss.cell.State != wire.StateActive { // a cell that is not admitted has no media to answer for
			ss.errorReply(f, wire.ErrNotAllowed, "cell is not active")
			return nil
		}
		a, err := wire.DecodeMediaOpenAck(f.Body)
		if err != nil {
			return malformed(err)
		}
		if ss.srv.Media != nil {
			ss.srv.Media.MediaOpened(ss.cell.ID, a)
		}
	case wire.MediaClosed:
		if ss.cell.State != wire.StateActive {
			ss.errorReply(f, wire.ErrNotAllowed, "cell is not active")
			return nil
		}
		c, err := wire.DecodeMediaClosed(f.Body)
		if err != nil {
			return malformed(err)
		}
		if ss.srv.Media != nil {
			ss.srv.Media.MediaClosed(ss.cell.ID, c)
		}
	case wire.TrustAck:
		// An answer to a request the controller does not send yet.
	case wire.Hello, wire.EnrollRequest:
		ss.errorReply(f, wire.ErrNotAllowed, "already established")
	default:
		// A controller-to-cell type sent by the cell.
		ss.errorReply(f, wire.ErrNotAllowed, "wrong direction")
	}
	return nil
}

func (ss *session) addRTT(d time.Duration) {
	ss.rtts = append(ss.rtts, d)
	if len(ss.rtts) > rttWindow {
		ss.rtts = ss.rtts[len(ss.rtts)-rttWindow:]
	}
	sorted := slices.Clone(ss.rtts)
	slices.Sort(sorted)
	p := func(q float64) time.Duration { return sorted[int(q*float64(len(sorted)-1))] }
	ss.srv.updateStatus(ss.cell.ID, func(st *Status) { st.RTTp50, st.RTTp95 = p(0.5), p(0.95) })
}

// hub: per-cell records, resume and orphan handling

// attach registers a new session for its cell, superseding an older one
// (§6.1), and decides whether it resumes the previous session (§6.4).
func (s *Server) attach(ss *session, h wire.HelloMsg) (resumed bool, token [16]byte, err error) {
	if _, err := rand.Read(token[:]); err != nil {
		return false, token, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.cells[ss.cell.ID]
	if rec == nil {
		rec = &record{}
		s.cells[ss.cell.ID] = rec
	}
	if old := rec.sess; old != nil {
		// The new connection wins; the old one is told and closed, and the
		// new one is treated as a resume attempt.
		old.sendNow(wire.Frame{Type: wire.Goodbye, Body: wire.GoodbyeMsg{Reason: wire.ByeSuperseded}.Encode()})
		old.shutdown()
		rec.status.Supersedes++
		rec.disconnected = time.Now()
	}
	inGrace := rec.sess != nil || (!rec.disconnected.IsZero() && time.Since(rec.disconnected) < s.Grace)
	resumed = h.ResumeToken != nil && h.Capabilities&wire.CapResume != 0 && inGrace &&
		subtle.ConstantTimeCompare(h.ResumeToken, rec.sessionToken[:]) == 1
	// The chip's subscription table did not survive a new boot or chip
	// restart (§7.4, HELLO tag 6).
	chipLost := rec.seenHello && (h.BootID != rec.bootID || h.ChipEpoch != rec.chipEpoch)
	if resumed && chipLost {
		resumed = false // §6.4: a changed chip_epoch makes it a fresh start
	}
	if !rec.seenHello || chipLost || h.RadioState != 1 {
		go func(id uuid.UUID) {
			if err := s.Store.SlotsUnknown(context.Background(), id); err != nil {
				s.Log.Warn("mark slots unknown", "cell", id, "err", err)
			}
		}(ss.cell.ID)
	}
	rec.bootID, rec.chipEpoch, rec.seenHello = h.BootID, h.ChipEpoch, true
	stopTimer(&rec.graceTimer)
	stopTimer(&rec.orphanTimer)
	rec.offline = false
	rec.sess, rec.sessionToken, rec.disconnected = ss, token, time.Time{}
	if resumed {
		rec.status.Resumes++
	}
	st := rec.status
	st.Online, st.Since, st.RemoteAddr, st.SWVersion = true, time.Now(), ss.addr.String(), h.SWVersion
	st.RadioState = h.RadioState
	st.ChipEpoch = h.ChipEpoch
	if h.HasRFPI { // a cell that sends no STATUS after its HELLO still shows what its chip broadcasts
		st.RFPI, st.HasRFPI = h.RFPI, true
	}
	st.AudioDown, st.AudioUp = h.AudioFormats, h.AudioFormatsUp
	if st.AudioUp == nil {
		st.AudioUp = h.AudioFormats // tag 16 absent: the same list
	}
	st.CapAudioNB, st.CapAudioWB = h.CapAudioNB, h.CapAudioWB
	rec.status = st
	return resumed, token, nil
}

func stopTimer(t **time.Timer) {
	if *t != nil {
		(*t).Stop()
		*t = nil
	}
}

// detach runs when a session ends: unless it was superseded, the grace and
// orphan timers start (§6.4).
func (s *Server) detach(ss *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.cells[ss.cell.ID]
	if rec == nil || rec.sess != ss {
		return // superseded: the newer session owns the record
	}
	rec.sess = nil
	rec.disconnected = time.Now()
	rec.status.Online = false
	id := ss.cell.ID
	w := s.watcher()
	if w != nil {
		w.CellDown(id)
	}
	rec.graceTimer = time.AfterFunc(s.Grace, func() {
		// The grace period is over: the cell's calls and DECT state end.
		s.mu.Lock()
		back := s.cells[id] != nil && s.cells[id].sess != nil
		s.mu.Unlock()
		if back {
			return
		}
		s.Log.Info("cell grace period expired", "cell", id)
		if w != nil {
			w.CellGone(id)
		}
	})
	rec.orphanTimer = time.AfterFunc(s.Orphan, func() {
		s.mu.Lock()
		if r := s.cells[id]; r != nil && r.sess == nil {
			r.offline = true
		}
		s.mu.Unlock()
		s.Log.Warn("cell offline", "cell", id)
		if err := s.Store.SlotsUnknown(context.Background(), id); err != nil {
			s.Log.Warn("mark slots unknown", "cell", id, "err", err)
		}
	})
}

func (s *Server) updateStatus(id uuid.UUID, fn func(*Status)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.cells[id]; rec != nil {
		fn(&rec.status)
	}
}

// CellStatus returns a cell's live status.
func (s *Server) CellStatus(id uuid.UUID) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.cells[id]; rec != nil {
		st := rec.status
		st.Stats = maps(st.Stats)
		return st
	}
	return Status{}
}

func maps(m map[string]uint64) map[string]uint64 {
	if m == nil {
		return nil
	}
	c := make(map[string]uint64, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// NotifyState tells a connected cell its new state (CELL_STATE), e.g. after
// an admin approved it.
func (s *Server) NotifyState(id uuid.UUID, st wire.CellStateValue, reason string) {
	s.withSession(id, func(ss *session) {
		was := ss.cell.State == wire.StateActive
		ss.cell.State = st
		ss.active.Store(st == wire.StateActive)
		ss.send(wire.Frame{Type: wire.CellState, Body: wire.CellStateMsg{State: st, Reason: reason}.Encode()})
		if w := s.watcher(); w != nil && was != (st == wire.StateActive) {
			if was {
				w.CellGone(id)
			} else {
				w.CellUp(id, ss.cellUp(false))
			}
		}
	})
}

// Kick closes a cell's link with GOODBYE REVOKED (§5.5).
func (s *Server) Kick(id uuid.UUID) {
	s.withSession(id, func(ss *session) {
		ss.sendNow(wire.Frame{Type: wire.Goodbye, Body: wire.GoodbyeMsg{Reason: wire.ByeRevoked}.Encode()})
		ss.shutdown()
	})
}

// withSession runs fn on the session's own goroutine.
func (s *Server) withSession(id uuid.UUID, fn func(*session)) {
	s.mu.Lock()
	var ss *session
	if rec := s.cells[id]; rec != nil {
		ss = rec.sess
	}
	s.mu.Unlock()
	if ss == nil {
		return
	}
	select {
	case ss.ctrl <- func() { fn(ss) }:
	case <-ss.done:
	case <-time.After(time.Second):
		ss.log.Warn("session busy; control message dropped")
	}
}
