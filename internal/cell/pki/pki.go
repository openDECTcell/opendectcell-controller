// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pki is the controller's private cell CA (docs/cell-protocol.md
// §4–5): it issues the controller's server certificate and per-cell client
// certificates, and encodes the enrollment string cells are given.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Validity periods (§4.1).
const (
	CAValidity     = 10 * 365 * 24 * time.Hour
	ServerValidity = 365 * 24 * time.Hour
	CellValidity   = 90 * 24 * time.Hour
	// Backdate covers small clock differences between controller and cell.
	Backdate = 5 * time.Minute
)

// CA is a loaded cell CA.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// ControllerURI is the SAN URI of the controller's server certificate:
// urn:opendectcell:controller:<installation uuid>. A cell requires it in the leaf
// and checks no hostname, so it can reach the controller by IP address
// (cell-protocol.md §4.2).
func ControllerURI(installation uuid.UUID) *url.URL {
	return &url.URL{Scheme: "urn", Opaque: "opendectcell:controller:" + installation.String()}
}

// CellURI is the SAN URI of a cell's client certificate,
// urn:opendectcell:cell:<cell uuid>; CellIDFromCert reads the id back from it.
func CellURI(cell uuid.UUID) *url.URL {
	return &url.URL{Scheme: "urn", Opaque: "opendectcell:cell:" + cell.String()}
}

// CellIDFromCert returns the cell UUID named by a certificate's SAN URI.
func CellIDFromCert(c *x509.Certificate) (uuid.UUID, error) {
	var found []uuid.UUID
	for _, u := range c.URIs {
		if u.Scheme == "urn" && strings.HasPrefix(u.Opaque, "opendectcell:cell:") {
			id, err := uuid.Parse(strings.TrimPrefix(u.Opaque, "opendectcell:cell:"))
			if err != nil {
				return uuid.Nil, fmt.Errorf("pki: bad cell URI %q", u)
			}
			found = append(found, id)
		}
	}
	if len(found) != 1 {
		return uuid.Nil, fmt.Errorf("pki: certificate names %d cells, want 1", len(found))
	}
	return found[0], nil
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// NewCA creates a self-signed P-256 CA for an installation.
func NewCA(installation uuid.UUID, now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "openDECTcell cell CA", OrganizationalUnit: []string{installation.String()}},
		NotBefore:             now.Add(-Backdate),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// LoadCA parses a CA from its certificate DER and PKCS #8 key DER.
func LoadCA(certDER, keyDER []byte) (*CA, error) {
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("pki: CA certificate: %w", err)
	}
	k, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("pki: CA key: %w", err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("pki: CA key does not match its certificate")
	}
	return &CA{Cert: cert, Key: key}, nil
}

// KeyDER returns the CA key as PKCS #8 DER, for encrypted storage.
func (ca *CA) KeyDER() ([]byte, error) { return x509.MarshalPKCS8PrivateKey(ca.Key) }

// Pin is the SHA-256 of the CA's SubjectPublicKeyInfo, base64url without
// padding: what the enrollment string carries (§5.1).
func (ca *CA) Pin() string { return SPKIPin(ca.Cert) }

// SPKIPin returns the pin of any certificate.
func SPKIPin(c *x509.Certificate) string {
	h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// Fingerprint is the SHA-256 of a certificate's DER, which the controller
// checks against the cell's current set (§4.2).
func Fingerprint(der []byte) []byte {
	h := sha256.Sum256(der)
	return h[:]
}

// ServerCert issues the controller's TLS server certificate with a fresh key.
func (ca *CA) ServerCert(installation uuid.UUID, hosts []string, now time.Time) (certDER []byte, key crypto.Signer, err error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: "openDECTcell controller"},
		NotBefore:    now.Add(-Backdate),
		NotAfter:     minTime(now.Add(ServerValidity), ca.Cert.NotAfter),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		URIs:         []*url.URL{ControllerURI(installation)},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &k.PublicKey, ca.Key)
	return der, k, err
}

// ErrBadCSR means a CSR was not acceptable.
var ErrBadCSR = errors.New("pki: CSR rejected")

// CheckCSR parses a CSR and checks its signature and key type (P-256 ECDSA,
// §5.2). The CSR's subject and extensions are ignored.
func CheckCSR(der []byte) (*ecdsa.PublicKey, error) {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadCSR, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: signature: %w", ErrBadCSR, err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: key must be ECDSA P-256", ErrBadCSR)
	}
	return pub, nil
}

// CellCert issues a client certificate for a cell from its checked key.
func (ca *CA) CellCert(cell uuid.UUID, pub *ecdsa.PublicKey, now time.Time) (*x509.Certificate, error) {
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: "openDECTcell cell " + cell.String()},
		NotBefore:    now.Add(-Backdate),
		NotAfter:     minTime(now.Add(CellValidity), ca.Cert.NotAfter),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{CellURI(cell)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Enrollment strings (§5.1):
//
//	opendectcell-enroll:v1?h=<host>:<port>&t=<token, base32>&ca=<SPKI SHA-256, base64url>

const enrollPrefix = "opendectcell-enroll:v1?"

var tokenEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// Enrollment is a parsed enrollment string.
type Enrollment struct {
	Addr  string // host:port
	Token []byte // 20 bytes
	Pin   string // base64url SHA-256 of the CA SPKI
}

// String encodes the enrollment string.
// The parameters are in the documented order, h, t, ca; base32 and
// base64url need no escaping.
func (e Enrollment) String() string {
	return enrollPrefix + "h=" + url.QueryEscape(e.Addr) + "&t=" + tokenEnc.EncodeToString(e.Token) + "&ca=" + e.Pin
}

// ParseEnrollment decodes an enrollment string.
func ParseEnrollment(s string) (Enrollment, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, enrollPrefix) {
		return Enrollment{}, errors.New("pki: not a opendectcell-enroll:v1 string")
	}
	v, err := url.ParseQuery(strings.TrimPrefix(s, enrollPrefix))
	if err != nil {
		return Enrollment{}, fmt.Errorf("pki: enrollment string: %w", err)
	}
	e := Enrollment{Addr: v.Get("h"), Pin: v.Get("ca")}
	host, port, err := net.SplitHostPort(e.Addr)
	if err != nil || host == "" {
		return Enrollment{}, fmt.Errorf("pki: enrollment address %q must be host:port", e.Addr)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return Enrollment{}, fmt.Errorf("pki: enrollment port %q", port)
	}
	if e.Token, err = tokenEnc.DecodeString(strings.ToUpper(v.Get("t"))); err != nil || len(e.Token) != 20 {
		return Enrollment{}, errors.New("pki: enrollment token must be 20 bytes of base32")
	}
	if pin, err := base64.RawURLEncoding.DecodeString(e.Pin); err != nil || len(pin) != sha256.Size {
		return Enrollment{}, errors.New("pki: CA pin must be a base64url SHA-256")
	}
	return e, nil
}

// VerifyPinned checks a TLS server chain the way a cell does (§4.2): the
// chain must end in a CA whose SPKI matches one of the pins, and the leaf
// must carry the controller URI. Hostnames are not checked.
func VerifyPinned(rawCerts [][]byte, pins []string, installation *uuid.UUID, now time.Time) error {
	if len(rawCerts) == 0 {
		return errors.New("pki: no server certificate")
	}
	certs := make([]*x509.Certificate, len(rawCerts))
	for i, r := range rawCerts {
		c, err := x509.ParseCertificate(r)
		if err != nil {
			return err
		}
		certs[i] = c
	}
	leaf := certs[0]
	lastErr := errors.New("pki: no pinned CA in the chain")
	for _, c := range certs[1:] {
		if !containsPin(pins, SPKIPin(c)) {
			continue
		}
		pool := x509.NewCertPool()
		pool.AddCert(c)
		_, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
	}
	if lastErr != nil {
		return lastErr
	}
	for _, u := range leaf.URIs {
		if u.Scheme == "urn" && strings.HasPrefix(u.Opaque, "opendectcell:controller:") &&
			(installation == nil || u.Opaque == "opendectcell:controller:"+installation.String()) {
			return nil
		}
	}
	return errors.New("pki: server certificate lacks the controller URI")
}

func containsPin(pins []string, p string) bool {
	for _, x := range pins {
		if x == p {
			return true
		}
	}
	return false
}
