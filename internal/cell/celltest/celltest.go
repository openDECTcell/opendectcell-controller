// SPDX-License-Identifier: AGPL-3.0-or-later

// Package celltest is a minimal cell for tests: it enrolls with an
// enrollment string, connects with its certificate, verifies the controller
// by CA pin only (docs/cell-protocol.md §4.2), and speaks HELLO and
// heartbeats. It doubles as a reference for the cell side.
package celltest

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell/pki"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
)

// Cell holds a cell's credentials.
type Cell struct {
	Addr   string
	Pins   []string
	Key    *ecdsa.PrivateKey
	Cert   *x509.Certificate
	ID     uuid.UUID
	State  wire.CellStateValue
	BootID [16]byte
}

func (c *Cell) tlsConfig(withCert bool) *tls.Config {
	conf := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The pin check replaces verification against system roots. It is
		// in VerifyConnection, not VerifyPeerCertificate, because only the
		// former also runs on resumed sessions (cell-protocol.md §4.2).
		InsecureSkipVerify: true, //nolint:gosec // G402: see above
		VerifyConnection: func(cs tls.ConnectionState) error {
			raw := make([][]byte, len(cs.PeerCertificates))
			for i, pc := range cs.PeerCertificates {
				raw[i] = pc.Raw
			}
			return pki.VerifyPinned(raw, c.Pins, nil, time.Now())
		},
	}
	if withCert && c.Cert != nil {
		conf.Certificates = []tls.Certificate{{Certificate: [][]byte{c.Cert.Raw}, PrivateKey: c.Key}}
	}
	return conf
}

func dial(ctx context.Context, addr string, conf *tls.Config) (*tls.Conn, error) {
	d := tls.Dialer{Config: conf, NetDialer: &net.Dialer{Timeout: 5 * time.Second}}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return c.(*tls.Conn), nil
}

// Enroll performs §5.2 and returns the response status. On success the
// cell's ID, certificate and state are set.
func Enroll(ctx context.Context, enrollment, nameHint string) (*Cell, uint16, error) {
	e, err := pki.ParseEnrollment(enrollment)
	if err != nil {
		return nil, 0, err
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c := &Cell{Addr: e.Addr, Pins: []string{e.Pin}, Key: key}
	rand.Read(c.BootID[:])
	status, err := c.enroll(ctx, e.Token, nameHint)
	return c, status, err
}

// EnrollWithToken retries an enrollment with an arbitrary token, for tests.
func (c *Cell) EnrollWithToken(ctx context.Context, token []byte) (uint16, error) {
	return c.enroll(ctx, token, "")
}

func (c *Cell) enroll(ctx context.Context, token []byte, nameHint string) (uint16, error) {
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, c.Key)
	if err != nil {
		return 0, err
	}
	conn, err := dial(ctx, c.Addr, c.tlsConfig(false))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	req := wire.EnrollRequestMsg{Token: token, CSR: csr, HWModel: "test cell", SWVersion: "celltest 1", NameHint: nameHint}
	b, _ := wire.Frame{Type: wire.EnrollRequest, Corr: 1, Body: req.Encode()}.Append(nil)
	if _, err := conn.Write(b); err != nil {
		return 0, err
	}
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	f, err := wire.ReadFrame(conn)
	if err != nil {
		return 0, err
	}
	if f.Type != wire.EnrollResponse {
		return 0, fmt.Errorf("celltest: got %s, want ENROLL_RESPONSE", f.Type)
	}
	resp, err := wire.DecodeEnrollResponse(f.Body)
	if err != nil || resp.Status != wire.EnrollOK {
		return resp.Status, err
	}
	ca, err := x509.ParseCertificate(resp.CACert)
	if err != nil {
		return 0, err
	}
	if pki.SPKIPin(ca) != c.Pins[0] {
		return 0, errors.New("celltest: CA certificate does not match the pin")
	}
	if c.Cert, err = x509.ParseCertificate(resp.Cert); err != nil {
		return 0, err
	}
	c.ID, c.State = resp.CellID, resp.State
	return resp.Status, nil
}

// Renew replaces the cell's key and certificate over an established link.
func (c *Cell) Renew(conn *Conn) error {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return err
	}
	if err := conn.Send(wire.Frame{Type: wire.RenewRequest, Corr: 9, Body: new(wire.Builder).Bytes(1, csr).Build()}); err != nil {
		return err
	}
	f, err := conn.Expect(wire.RenewResponse, 5*time.Second)
	if err != nil {
		return err
	}
	t, err := wire.ParseTLV(f.Body)
	if err != nil {
		return err
	}
	if st, _ := t.U16(1); st != 0 {
		return fmt.Errorf("celltest: renewal status %d", st)
	}
	der, _ := t.Get(2)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	c.Key, c.Cert = key, cert
	return nil
}

// Options shape a connection.
type Options struct {
	ResumeToken []byte
	ChipEpoch   uint32
	Major       uint8    // default wire.ProtoMajor
	NoAutoEcho  bool     // don't answer the controller's heartbeats
	NoHello     bool     // connect but send nothing
	RFPI        *[5]byte // the identity the chip broadcasts, in HELLO tag 17
	Media       bool     // announce MEDIA and SRTP with audio formats L16 8 kHz and G.722, as a real cell does
}

// Conn is an established link.
type Conn struct {
	tls    *tls.Conn
	Ack    wire.HelloAckMsg
	frames chan wire.Frame
	err    chan error
	wmu    sync.Mutex
}

// Connect opens the control link and completes the handshake.
func (c *Cell) Connect(ctx context.Context, o Options) (*Conn, error) {
	conn, err := dial(ctx, c.Addr, c.tlsConfig(true))
	if err != nil {
		return nil, err
	}
	cn := &Conn{tls: conn, frames: make(chan wire.Frame, 64), err: make(chan error, 1)}
	if o.NoHello {
		go cn.read(o)
		return cn, nil
	}
	major := o.Major
	if major == 0 {
		major = wire.ProtoMajor
	}
	h := wire.HelloMsg{Major: major, SWVersion: "celltest 1", BootID: c.BootID, ChipEpoch: o.ChipEpoch,
		Capabilities: wire.CapMail | wire.CapStats | wire.CapFlow | wire.CapResume, CapSubRecords: 6,
		CapRadioContexts: 6, CapAudioNB: 4, CapAudioWB: 4, ResumeToken: o.ResumeToken, RadioState: 1}
	if o.RFPI != nil {
		h.RFPI, h.HasRFPI = *o.RFPI, true
	}
	if o.Media {
		h.Capabilities |= wire.CapMedia | wire.CapSRTP
		h.AudioFormats = []uint8{wire.FormatL16_8k, wire.FormatG722}
	}
	if err := cn.Send(wire.Frame{Type: wire.Hello, Corr: 1, Body: h.Encode()}); err != nil {
		conn.Close()
		return nil, err
	}
	go cn.read(o)
	f, err := cn.Next(5 * time.Second)
	if err != nil {
		conn.Close()
		return nil, err
	}
	switch f.Type {
	case wire.HelloAck:
	case wire.Goodbye:
		g, _ := wire.DecodeGoodbye(f.Body)
		conn.Close()
		return nil, &GoodbyeError{g}
	default:
		conn.Close()
		return nil, fmt.Errorf("celltest: got %s, want HELLO_ACK", f.Type)
	}
	if cn.Ack, err = wire.DecodeHelloAck(f.Body); err != nil {
		conn.Close()
		return nil, err
	}
	return cn, nil
}

// GoodbyeError is a GOODBYE received instead of an answer.
type GoodbyeError struct{ Msg wire.GoodbyeMsg }

func (e *GoodbyeError) Error() string {
	return fmt.Sprintf("celltest: GOODBYE %d %q", e.Msg.Reason, e.Msg.Text)
}

func (cn *Conn) read(o Options) {
	r := bufio.NewReader(cn.tls)
	for {
		f, err := wire.ReadFrame(r)
		if err != nil {
			cn.err <- err
			close(cn.frames)
			return
		}
		if f.Type == wire.Heartbeat && !o.NoAutoEcho {
			_ = cn.Send(wire.Frame{Type: wire.HeartbeatEcho, Corr: f.Corr, Body: f.Body})
			continue
		}
		cn.frames <- f
	}
}

// Send writes one frame.
func (cn *Conn) Send(f wire.Frame) error {
	b, err := f.Append(nil)
	if err != nil {
		return err
	}
	cn.wmu.Lock()
	defer cn.wmu.Unlock()
	_, err = cn.tls.Write(b)
	return err
}

// ErrClosed means the controller closed the link.
var ErrClosed = errors.New("celltest: link closed")

// Next returns the next frame other than an auto-answered heartbeat.
func (cn *Conn) Next(timeout time.Duration) (wire.Frame, error) {
	select {
	case f, ok := <-cn.frames:
		if !ok {
			return wire.Frame{}, ErrClosed
		}
		return f, nil
	case <-time.After(timeout):
		return wire.Frame{}, errors.New("celltest: timeout")
	}
}

// Expect skips frames until one of type t arrives.
func (cn *Conn) Expect(t wire.Type, timeout time.Duration) (wire.Frame, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := cn.Next(time.Until(deadline))
		if err != nil {
			return f, err
		}
		if f.Type == t {
			return f, nil
		}
	}
}

// WaitClosed waits until the controller closes the link, returning any
// GOODBYE it sent first.
func (cn *Conn) WaitClosed(timeout time.Duration) (*wire.GoodbyeMsg, error) {
	deadline := time.Now().Add(timeout)
	var bye *wire.GoodbyeMsg
	for {
		f, err := cn.Next(time.Until(deadline))
		if errors.Is(err, ErrClosed) {
			return bye, nil
		}
		if err != nil {
			return bye, err
		}
		if f.Type == wire.Goodbye {
			g, _ := wire.DecodeGoodbye(f.Body)
			bye = &g
		}
	}
}

// Close closes the link without GOODBYE (like a lost connection).
func (cn *Conn) Close() error { return cn.tls.Close() }
