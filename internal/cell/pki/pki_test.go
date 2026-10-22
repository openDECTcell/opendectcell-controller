// SPDX-License-Identifier: AGPL-3.0-or-later

package pki

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var now = time.Now()

func csr(t *testing.T, curve elliptic.Curve) ([]byte, *ecdsa.PrivateKey) {
	k, _ := ecdsa.GenerateKey(curve, rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, k)
	if err != nil {
		t.Fatal(err)
	}
	return der, k
}

func TestCAStoreAndLoad(t *testing.T) {
	inst := uuid.New()
	ca, err := NewCA(inst, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.Cert.IsCA || ca.Cert.MaxPathLen != 0 || !ca.Cert.MaxPathLenZero {
		t.Error("CA constraints")
	}
	kd, _ := ca.KeyDER()
	back, err := LoadCA(ca.Cert.Raw, kd)
	if err != nil || back.Pin() != ca.Pin() {
		t.Fatalf("LoadCA: %v", err)
	}
	other, _ := NewCA(inst, now)
	okd, _ := other.KeyDER()
	if _, err := LoadCA(ca.Cert.Raw, okd); err == nil {
		t.Error("mismatched key accepted")
	}
}

func TestCellCertFromCSR(t *testing.T) {
	ca, _ := NewCA(uuid.New(), now)
	der, _ := csr(t, elliptic.P256())
	pub, err := CheckCSR(der)
	if err != nil {
		t.Fatal(err)
	}
	cell := uuid.New()
	c, err := ca.CellCert(cell, pub, now)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := CellIDFromCert(c); err != nil || id != cell {
		t.Fatalf("CellIDFromCert: %v %v", id, err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Error("cell certificate usable as a server certificate")
	}
	if d := c.NotAfter.Sub(now); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Errorf("validity %v", d)
	}

	p384, _ := csr(t, elliptic.P384())
	bad := append([]byte(nil), der...)
	bad[len(bad)-3] ^= 0xFF
	for name, in := range map[string][]byte{"P-384": p384, "bad signature": bad, "garbage": {1, 2, 3}} {
		if _, err := CheckCSR(in); !errors.Is(err, ErrBadCSR) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestEnrollmentString(t *testing.T) {
	ca, _ := NewCA(uuid.New(), now)
	e := Enrollment{Addr: "dect.example.org:7300", Token: bytes.Repeat([]byte{0xAB}, 20), Pin: ca.Pin()}
	s := e.String()
	if !strings.HasPrefix(s, "opendectcell-enroll:v1?h=dect.example.org%3A7300&t=") {
		t.Fatalf("format: %s", s)
	}
	got, err := ParseEnrollment("  " + s + "\n")
	if err != nil || got.Addr != e.Addr || !bytes.Equal(got.Token, e.Token) || got.Pin != e.Pin {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, err := ParseEnrollment("opendectcell-enroll:v1?h=[2001:db8::1]:7300&t=" + tokenEnc.EncodeToString(e.Token) + "&ca=" + e.Pin); err != nil {
		t.Errorf("IPv6: %v", err)
	}
	for _, bad := range []string{
		"", "opendectcell-enroll:v2?h=a:1", strings.Replace(s, "7300", "0", 1),
		strings.Replace(s, "&ca=", "&ca=x", 1), "opendectcell-enroll:v1?h=a:1&t=AAAA&ca=" + e.Pin,
	} {
		if _, err := ParseEnrollment(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestPinnedMutualTLS runs a real TLS 1.3 handshake in both directions: the
// cell verifies the controller by pin only, the controller requires and
// checks the cell certificate.
func TestPinnedMutualTLS(t *testing.T) {
	inst := uuid.New()
	ca, _ := NewCA(inst, now)
	sder, skey, err := ca.ServerCert(inst, []string{"dect.example.org", "192.0.2.1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	cder, ckey := csr(t, elliptic.P256())
	pub, _ := CheckCSR(cder)
	cell := uuid.New()
	cc, _ := ca.CellCert(cell, pub, now)

	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	srv := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{sder, ca.Cert.Raw}, PrivateKey: skey}},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	cli := func(pins []string) *tls.Config {
		return &tls.Config{
			MinVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true, // replaced by the pin check below
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				return VerifyPinned(raw, pins, &inst, time.Now())
			},
			Certificates: []tls.Certificate{{Certificate: [][]byte{cc.Raw}, PrivateKey: ckey}},
		}
	}
	handshake := func(c *tls.Config) (uuid.UUID, error) {
		a, b := net.Pipe()
		errc := make(chan error, 1)
		var id uuid.UUID
		go func() {
			s := tls.Server(a, srv)
			err := s.Handshake()
			if err == nil {
				id, err = CellIDFromCert(s.ConnectionState().PeerCertificates[0])
			}
			s.Close()
			errc <- err
		}()
		cerr := tls.Client(b, c).Handshake()
		b.Close()
		serr := <-errc
		if cerr != nil {
			return id, cerr
		}
		return id, serr
	}
	id, err := handshake(cli([]string{ca.Pin()}))
	if err != nil || id != cell {
		t.Fatalf("pinned handshake: %v %v", id, err)
	}
	other, _ := NewCA(inst, now)
	if _, err := handshake(cli([]string{other.Pin()})); err == nil {
		t.Fatal("handshake succeeded with the wrong pin")
	}
}
