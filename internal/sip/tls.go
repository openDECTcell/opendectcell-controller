// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
)

// tlsVerifier checks SIP-over-TLS server certificates (voip-requirements.md
// §7, RFC 5922). sipgo shares one TLS configuration across all accounts,
// so before connecting, each account registers what it expects from a
// server: the names its certificate may carry (the SIP domain and the SRV
// target) and whether to verify at all. Expectations are keyed by the
// server name we dial (also sent as SNI). Servers configured as an IP
// literal have no name: their expectations share the key "" (any of their
// domains is accepted; verification is on if any account asks for it).
type tlsVerifier struct {
	roots *x509.CertPool // nil: the system roots

	mu     sync.Mutex
	expect map[string]tlsExpect
}

type tlsExpect struct {
	names  map[string]bool
	verify bool
}

// tlsKey is the expectation key for a dialled host.
func tlsKey(host string) string {
	if _, err := netip.ParseAddr(host); err == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func (v *tlsVerifier) expectFor(host string, verify bool, names ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.expect == nil {
		v.expect = map[string]tlsExpect{}
	}
	k := tlsKey(host)
	e, ok := v.expect[k]
	if !ok {
		e = tlsExpect{names: map[string]bool{}, verify: verify}
	}
	// Conservative: if any account using this server verifies, verify.
	e.verify = e.verify || verify
	for _, n := range names {
		if n != "" {
			e.names[n] = true
		}
	}
	v.expect[k] = e
}

// config is the TLS client configuration sipgo dials with.
func (v *tlsVerifier) config() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Verification happens in VerifyConnection, which also runs on
		// resumed sessions; the default check would compare the
		// certificate with an IP address.
		InsecureSkipVerify: true, //nolint:gosec // G402: replaced by verifyConnection
		VerifyConnection:   v.verifyConnection,
	}
}

func (v *tlsVerifier) verifyConnection(cs tls.ConnectionState) error {
	k := tlsKey(cs.ServerName)
	v.mu.Lock()
	e, ok := v.expect[k]
	v.mu.Unlock()
	if !ok {
		return fmt.Errorf("sip: TLS to %q: no account expects this server", cs.ServerName)
	}
	if !e.verify {
		return nil
	}
	if len(cs.PeerCertificates) == 0 {
		return errors.New("sip: TLS server sent no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	leaf := cs.PeerCertificates[0]
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: v.roots, Intermediates: inter}); err != nil {
		return fmt.Errorf("sip: TLS certificate: %w", err)
	}
	for n := range e.names {
		if leaf.VerifyHostname(n) == nil {
			return nil
		}
	}
	return fmt.Errorf("sip: TLS certificate of %q is not valid for %v", cs.ServerName, keys(e.names))
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
