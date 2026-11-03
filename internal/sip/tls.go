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
// §7, RFC 5922). sipgo shares one TLS configuration across all accounts, and
// the check cannot see which account a connection serves, only the server
// name we dialled (sent as SNI). So every account registers what it expects
// from the server it uses: the names its certificate may carry and whether to
// verify at all. A connection is accepted only if the certificate satisfies the
// expectation of every verifying account that uses that server: a certificate
// that is good for account A does not stand in for B's identity.
//
// A name is the key of the accounts that dial it. A server configured as an IP
// literal has no name, so every such account shares the key "": they must all
// be satisfied by one certificate, and a second identity makes the connection
// fail with a message that says so (use a host name instead).
//
// The names an account accepts are its SIP domain and the registrar or
// outbound proxy it was configured with; not a name that only a DNS SRV record
// named, as DNS answers are not authenticated (RFC 5922 §7.2).
type tlsVerifier struct {
	roots *x509.CertPool // nil: the system roots

	mu     sync.Mutex
	expect map[string]tlsExpect // by account
}

type tlsExpect struct {
	key    string // the dialled server, see tlsKey
	names  []string
	verify bool
}

// tlsKey is the expectation key for a dialled host: its name, lower case, or ""
// for an IP literal (the TLS state carries no server name then).
func tlsKey(host string) string {
	if _, err := netip.ParseAddr(host); err == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// expectFor sets what account acct expects of the server host; it replaces
// what the account expected before.
func (v *tlsVerifier) expectFor(acct, host string, verify bool, names ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.expect == nil {
		v.expect = map[string]tlsExpect{}
	}
	e := tlsExpect{key: tlsKey(host), verify: verify}
	for _, n := range names {
		if n != "" {
			e.names = append(e.names, strings.ToLower(n))
		}
	}
	v.expect[acct] = e
}

// forget drops an account's expectation (the account is gone).
func (v *tlsVerifier) forget(acct string) {
	v.mu.Lock()
	delete(v.expect, acct)
	v.mu.Unlock()
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
	var users, strict []tlsExpect
	for _, e := range v.expect {
		if e.key == k {
			users = append(users, e)
			if e.verify {
				strict = append(strict, e)
			}
		}
	}
	v.mu.Unlock()
	if len(users) == 0 {
		return fmt.Errorf("sip: TLS to %q: no account expects this server", cs.ServerName)
	}
	if len(strict) == 0 {
		return nil // every account here asked for no verification
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
	for _, e := range strict {
		ok := false
		for _, n := range e.names {
			if leaf.VerifyHostname(n) == nil {
				ok = true
				break
			}
		}
		if !ok {
			msg := fmt.Sprintf("sip: TLS certificate of %q is not valid for %v", cs.ServerName, sortedCopy(e.names))
			if k == "" && len(strict) > 1 {
				msg += " (accounts that name their server by IP address share one certificate identity: give the server as a host name)"
			}
			return errors.New(msg)
		}
	}
	return nil
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
