// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"
)

// tlsFixture makes certificates from one test CA.
type tlsFixture struct {
	t    *testing.T
	ca   *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
	n    int64
}

func newTLSFixture(t *testing.T) *tlsFixture {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tlsFixture{t: t, ca: ca, key: key, pool: pool, n: 1}
}

// state is what a server with a certificate for names presents when it was
// dialled as server.
func (f *tlsFixture) state(server string, names ...string) tls.ConnectionState {
	f.t.Helper()
	f.n++
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(f.n), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.ca, &leafKey.PublicKey, f.key)
	if err != nil {
		f.t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.ConnectionState{ServerName: server, PeerCertificates: []*x509.Certificate{leaf}}
}

// A certificate that is good for one account's identity does not stand in for
// another account's that dials the same server.
func TestTLSCertificateOfOneAccountDoesNotSatisfyAnother(t *testing.T) {
	f := newTLSFixture(t)
	v := &tlsVerifier{roots: f.pool}
	v.expectFor("A", "sip.example.net", true, "a.example")
	v.expectFor("B", "sip.example.net", true, "b.example")

	if err := v.verifyConnection(f.state("sip.example.net", "a.example")); err == nil || !strings.Contains(err.Error(), "b.example") {
		t.Fatalf("a certificate for A alone was accepted for B's server: %v", err)
	}
	if err := v.verifyConnection(f.state("sip.example.net", "b.example")); err == nil || !strings.Contains(err.Error(), "a.example") {
		t.Fatalf("a certificate for B alone was accepted for A's server: %v", err)
	}
	if err := v.verifyConnection(f.state("sip.example.net", "a.example", "b.example")); err != nil {
		t.Fatalf("a certificate for both was refused: %v", err)
	}

	// B goes away: A's certificate is enough again; nothing of B lingers.
	v.forget("B")
	if err := v.verifyConnection(f.state("sip.example.net", "a.example")); err != nil {
		t.Fatalf("after forget: %v", err)
	}
	// An account that changed its server no longer counts on the old one.
	v.expectFor("A", "other.example.net", true, "a.example")
	if err := v.verifyConnection(f.state("sip.example.net", "unrelated.example")); err == nil || !strings.Contains(err.Error(), "no account expects") {
		t.Fatalf("a server nobody uses any more was accepted: %v", err)
	}
}

// An account that does not verify rides along; one that does decides.
func TestTLSMixedVerification(t *testing.T) {
	f := newTLSFixture(t)
	v := &tlsVerifier{roots: f.pool}
	v.expectFor("lax", "sip.example.net", false, "lax.example")
	if err := v.verifyConnection(f.state("sip.example.net", "whatever.example")); err != nil {
		t.Fatalf("no account verifies, yet refused: %v", err)
	}
	v.expectFor("strict", "sip.example.net", true, "strict.example")
	if err := v.verifyConnection(f.state("sip.example.net", "whatever.example")); err == nil {
		t.Fatal("a certificate for another name was accepted for a verifying account")
	}
	if err := v.verifyConnection(f.state("sip.example.net", "strict.example")); err != nil {
		t.Fatalf("the verifying account's certificate was refused: %v", err)
	}
	// A certificate from an unknown CA is refused even if the name fits.
	other := newTLSFixture(t)
	if err := v.verifyConnection(other.state("sip.example.net", "strict.example")); err == nil {
		t.Fatal("a certificate from another CA was accepted")
	}
}

// Accounts at IP-literal servers share the key "": they need one certificate,
// and the error says what to do.
func TestTLSIPLiteralServersShareOneIdentity(t *testing.T) {
	f := newTLSFixture(t)
	v := &tlsVerifier{roots: f.pool}
	v.expectFor("A", "203.0.113.7", true, "a.example")
	if err := v.verifyConnection(f.state("", "a.example")); err != nil {
		t.Fatalf("one identity: %v", err)
	}
	v.expectFor("B", "203.0.113.9", true, "b.example")
	err := v.verifyConnection(f.state("", "a.example"))
	if err == nil || !strings.Contains(err.Error(), "host name") {
		t.Fatalf("a second identity behind IP addresses was not refused with advice: %v", err)
	}
}

func TestHostOfIgnoresAddresses(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "sip.example.net": "sip.example.net", "sip.example.net:5061": "sip.example.net",
		"203.0.113.7": "", "203.0.113.7:5061": "", "[2001:db8::1]:5061": "", "2001:db8::1": "",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
