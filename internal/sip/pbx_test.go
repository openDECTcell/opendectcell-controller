// SPDX-License-Identifier: AGPL-3.0-or-later

package sip_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"

	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

// pbx is a small fake PBX for tests: a registrar with digest auth, a UAS
// that answers the controller's calls, and a UAC that calls it.
type pbx struct {
	t       *testing.T
	addr    netip.AddrPort
	ua      *sipgo.UserAgent
	srv     *sipgo.Server
	client  *sipgo.Client
	dialogs *sipgo.DialogUA
	rtp     *rtp.Ports
	conn    net.PacketConn

	mu         sync.Mutex
	users      map[string]string  // user → password
	algos      []string           // challenge algorithms, in header order
	bindings   map[string]sip.Uri // user → contact
	registers  map[string]int     // user → successful REGISTERs
	expires    int                // what the registrar grants
	authInvite bool               // challenge INVITEs with 407
	answer     func(*inCall)      // what to do with an INVITE from the controller
	calls      chan *inCall       // INVITEs from the controller
	seenVia    []string
	byes       chan string // Call-IDs of BYEs received
	reinvites  chan *sip.Request
	infos      chan *sip.Request
	nonce      string
	sessions   map[string]*sipgo.DialogServerSession // by Call-ID, for ACKs
}

// inCall is an INVITE the PBX received from the controller.
type inCall struct {
	p     *pbx
	req   *sip.Request
	sess  *sipgo.DialogServerSession
	media *rtp.Endpoint
	neg   *sdp.Media
	body  []byte
}

func newPBX(t *testing.T) *pbx {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("fake-pbx"))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, _ := sipgo.NewServer(ua, sipgo.WithServerLogger(quiet))
	cl, _ := sipgo.NewClient(ua, sipgo.WithClientLogger(quiet), sipgo.WithClientHostname("127.0.0.1"),
		sipgo.WithClientPort(conn.LocalAddr().(*net.UDPAddr).Port))
	p := &pbx{t: t, addr: conn.LocalAddr().(*net.UDPAddr).AddrPort(), ua: ua, srv: srv, client: cl, conn: conn,
		users: map[string]string{}, algos: []string{"MD5"}, bindings: map[string]sip.Uri{}, registers: map[string]int{},
		expires: 3600, calls: make(chan *inCall, 8), byes: make(chan string, 8), reinvites: make(chan *sip.Request, 8),
		infos: make(chan *sip.Request, 8), nonce: randHex(), sessions: map[string]*sipgo.DialogServerSession{}, rtp: &rtp.Ports{IP: netip.MustParseAddr("127.0.0.1"), Min: 43000, Max: 43199}}
	contact := sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "pbx", Host: "127.0.0.1", Port: int(p.addr.Port())}}
	p.dialogs = &sipgo.DialogUA{Client: cl, ContactHDR: contact, RewriteContact: true}
	srv.OnRegister(p.onRegister)
	srv.OnInvite(p.onInvite)
	srv.OnAck(p.onAck)
	srv.OnBye(p.onBye)
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})
	srv.OnInfo(func(req *sip.Request, tx sip.ServerTransaction) {
		tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
		p.infos <- req
	})
	go srv.ServeUDP(conn)
	t.Cleanup(func() { srv.Close(); cl.Close(); ua.Close(); conn.Close() })
	return p
}

// set changes the PBX's configuration under its lock.
func (p *pbx) set(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn()
}

func randHex() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (p *pbx) challenge(algo string) *digest.Challenge {
	return &digest.Challenge{Realm: "pbx.test", Nonce: p.nonce, Algorithm: algo, QOP: []string{"auth"}}
}

// authorised checks the request's credentials against every algorithm the
// PBX offered. It returns the algorithm used, or "" if none verified.
func (p *pbx) authorised(req *sip.Request, hdr string) (string, string) {
	h := req.GetHeader(hdr)
	if h == nil {
		return "", ""
	}
	cr, err := digest.ParseCredentials(h.Value())
	if err != nil {
		return "", ""
	}
	p.mu.Lock()
	pw, ok := p.users[cr.Username]
	p.mu.Unlock()
	if !ok || cr.Nonce != p.nonce {
		return "", ""
	}
	algo := cr.Algorithm
	if algo == "" {
		algo = "MD5"
	}
	want, err := digest.Digest(p.challenge(algo), digest.Options{Method: req.Method.String(), URI: cr.URI, Username: cr.Username,
		Password: pw, Cnonce: cr.Cnonce, Count: cr.Nc})
	if err != nil || want.Response != cr.Response {
		return "", ""
	}
	return cr.Username, algo
}

func (p *pbx) onRegister(req *sip.Request, tx sip.ServerTransaction) {
	p.mu.Lock()
	p.seenVia = append(p.seenVia, req.Via().Value())
	algos := append([]string(nil), p.algos...)
	exp := p.expires
	p.mu.Unlock()
	user, algo := p.authorised(req, "Authorization")
	if user == "" {
		res := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
		for _, a := range algos {
			res.AppendHeader(sip.NewHeader("WWW-Authenticate", p.challenge(a).String()))
		}
		tx.Respond(res)
		return
	}
	ct := req.Contact()
	reqExp := exp
	if e := req.GetHeader("Expires"); e != nil {
		if n, err := strconv.Atoi(e.Value()); err == nil && n < reqExp {
			reqExp = n
		}
	}
	p.mu.Lock()
	if reqExp == 0 {
		delete(p.bindings, user)
	} else {
		p.bindings[user] = ct.Address
		p.registers[user+"/"+algo]++
		p.registers[user]++
	}
	p.mu.Unlock()
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	if reqExp > 0 {
		c := ct.Clone()
		c.Params = sip.NewParams()
		c.Params.Add("expires", strconv.Itoa(reqExp))
		res.AppendHeader(c)
	}
	tx.Respond(res)
}

func (p *pbx) registered(user string) (sip.Uri, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bindings[user], p.registers[user]
}

func (p *pbx) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	if to := req.To(); to != nil {
		if _, ok := to.Params.Get("tag"); ok { // re-INVITE: answer with what we had
			p.reinvites <- req
			ans, _, err := sdp.Answer(req.Body(), sdp.Local{Addr: netip.MustParseAddrPort("127.0.0.1:43500"), Codecs: []string{"G722", "PCMA"}, Encryption: sdp.EncOptional})
			if err != nil {
				tx.Respond(sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
				return
			}
			tx.Respond(sip.NewSDPResponseFromRequest(req, ans))
			return
		}
	}
	p.mu.Lock()
	authInv := p.authInvite
	p.mu.Unlock()
	if authInv {
		if user, _ := p.authorised(req, "Proxy-Authorization"); user == "" {
			res := sip.NewResponseFromRequest(req, 407, "Proxy Authentication Required", nil)
			res.AppendHeader(sip.NewHeader("Proxy-Authenticate", p.challenge("MD5").String()))
			tx.Respond(res)
			return
		}
	}
	sess, err := p.dialogs.ReadInvite(req, tx)
	if err != nil {
		tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
		return
	}
	c := &inCall{p: p, req: req, sess: sess}
	p.mu.Lock()
	p.sessions[req.CallID().Value()] = sess
	p.mu.Unlock()
	p.calls <- c
	p.mu.Lock()
	answer := p.answer
	p.mu.Unlock()
	if answer != nil {
		answer(c)
	}
	<-sess.Context().Done()
}

// answerCall answers an INVITE from the controller with media.
func (c *inCall) answerCall(t *testing.T, enc sdp.Encryption) {
	ep, err := c.p.rtp.Open(context.Background())
	if err != nil {
		t.Error(err)
		return
	}
	c.media = ep
	ans, neg, err := sdp.Answer(c.req.Body(), sdp.Local{Addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(ep.LocalPort())),
		Codecs: []string{"G722", "PCMA"}, Encryption: enc})
	if err != nil {
		c.sess.Respond(488, "Not Acceptable Here", nil)
		return
	}
	ep.Configure(neg)
	c.neg, c.body = neg, ans
	c.sess.Respond(180, "Ringing", nil)
	go func() {
		time.Sleep(50 * time.Millisecond) // like a person picking up
		c.sess.RespondSDP(ans)
	}()
}

func (p *pbx) onAck(req *sip.Request, tx sip.ServerTransaction) {
	p.mu.Lock()
	s := p.sessions[req.CallID().Value()]
	p.mu.Unlock()
	if s != nil {
		s.ReadAck(req, tx)
	}
}

func (p *pbx) onBye(req *sip.Request, tx sip.ServerTransaction) {
	tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	p.byes <- req.CallID().Value()
}

// call places a call from the PBX to a registered user.
func (p *pbx) call(t *testing.T, ctx context.Context, user string, offer []byte, extra ...sip.Header) (*sipgo.DialogClientSession, error) {
	contact, _ := p.registered(user)
	if contact.Host == "" {
		t.Fatalf("%s is not registered", user)
	}
	req := sip.NewRequest(sip.INVITE, contact)
	req.SetBody(offer)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	from := &sip.FromHeader{DisplayName: "Alice", Address: sip.Uri{Scheme: "sip", User: "+4930123456", Host: "pbx.test"}, Params: sip.NewParams()}
	from.Params.Add("tag", randHex())
	req.AppendHeader(from)
	req.AppendHeader(&sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: user, Host: "pbx.test"}, Params: sip.NewParams()})
	for _, h := range extra {
		req.AppendHeader(h)
	}
	return p.dialogs.WriteInvite(ctx, req)
}

// listenTLS adds a TLS listener whose certificate (from a fresh test CA)
// names the given hosts. It returns the CA pool and the listener address.
func (p *pbx) listenTLS(t *testing.T, names ...string) (*x509.CertPool, netip.AddrPort) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der, caDER}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	go p.srv.ServeTLS(ln)
	t.Cleanup(func() { ln.Close() })
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool, ln.Addr().(*net.TCPAddr).AddrPort()
}
