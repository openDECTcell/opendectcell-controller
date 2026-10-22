// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sip is the controller's SIP side (ROADMAP C4): one logical user
// agent per SIP account, registrations, calls in and out, hold and DTMF
// (docs/sip-ua.md). The DECT side reaches it through Endpoint and Call.
package sip

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

// Account is a SIP account as the SIP side needs it, with the password in
// clear (it lives only in memory; the database copy is sealed).
type Account struct {
	ID             uuid.UUID
	Label          string
	DisplayName    string
	Username       string // AOR user part
	Domain         string // AOR host
	AuthUsername   string // "" = Username
	Password       string
	Registrar      string // host[:port]; "" = Domain via RFC 3263
	OutboundProxy  string // host[:port]; loose route for every request
	Transport      resolve.Transport
	Register       bool
	RegisterExpiry int // seconds asked for
	Keepalive      string
	Encryption     sdp.Encryption
	Codecs         []string
	DTMFMode       string // rfc4733, info, inband
	CallerIDSource string // auto, from, pai, rpid
	TLSVerify      bool
	Enabled        bool
	UserAgent      string // advanced.user_agent
}

// Validate checks what the SIP side relies on.
func (a Account) Validate() error {
	var errs []error
	if a.Username == "" || strings.ContainsAny(a.Username, " \t\r\n<>@;") {
		errs = append(errs, errors.New("user must be a plain SIP user part"))
	}
	if a.Domain == "" || strings.ContainsAny(a.Domain, " \t\r\n<>@;") {
		errs = append(errs, errors.New("domain must be a host name or address"))
	}
	for _, hp := range []string{a.Registrar, a.OutboundProxy} {
		if hp != "" {
			if _, _, err := resolve.ParseHostPort(hp); err != nil {
				errs = append(errs, err)
			}
		}
	}
	switch a.Transport {
	case resolve.UDP, resolve.TCP, resolve.TLS:
	default:
		errs = append(errs, fmt.Errorf("transport %q must be udp, tcp or tls", a.Transport))
	}
	if a.Register && (a.RegisterExpiry < 60 || a.RegisterExpiry > 86400) {
		errs = append(errs, errors.New("registration expiry must be 60 to 86400 seconds"))
	}
	if len(a.Codecs) == 0 {
		errs = append(errs, errors.New("at least one codec is required"))
	}
	for _, c := range a.Codecs {
		if _, ok := sdp.Codecs[c]; !ok {
			errs = append(errs, fmt.Errorf("codec %q is not supported", c))
		}
	}
	return errors.Join(errs...)
}

func (a Account) authUser() string {
	if a.AuthUsername != "" {
		return a.AuthUsername
	}
	return a.Username
}

// ContactUser is the account's own Contact user part. Incoming requests are
// routed to the account by it (docs/sip-ua.md §2).
func ContactUser(id uuid.UUID) string {
	return "dw-" + strings.ReplaceAll(id.String(), "-", "")[:16]
}

// Warnings returns configuration problems that are allowed but unwise,
// for the UI (docs/api/openapi.yaml, SipAccount.warnings).
func (a Account) Warnings() []string {
	var w []string
	if a.Encryption != sdp.EncOff && a.Transport != resolve.TLS {
		w = append(w, "srtp_without_tls")
	}
	if a.Transport == resolve.TLS && !a.TLSVerify {
		w = append(w, "tls_not_verified")
	}
	return w
}
