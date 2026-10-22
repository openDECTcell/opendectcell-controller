// SPDX-License-Identifier: AGPL-3.0-or-later

// Package resolve locates SIP servers as RFC 3263 §4 describes: NAPTR, then
// SRV, then A/AAAA, producing an ordered list of targets to try in turn
// (docs/sip-ua.md §2).
package resolve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// Transport is a SIP transport.
type Transport string

const (
	UDP Transport = "udp"
	TCP Transport = "tcp"
	TLS Transport = "tls"
)

// Target is one place to send a request.
type Target struct {
	Transport Transport
	Addr      netip.AddrPort
	// Host is the name the target was found under, for TLS server name
	// verification (RFC 5922 uses the domain of the URI, see ServerName).
	Host string
}

func (t Target) String() string { return string(t.Transport) + ":" + t.Addr.String() }

// NAPTR is the subset of a NAPTR record that SIP uses.
type NAPTR struct {
	Order, Preference uint16
	Flags, Service    string
	Replacement       string
}

// DNS is what the resolver needs from DNS. Production uses System; tests
// use a fake.
type DNS interface {
	LookupNAPTR(ctx context.Context, name string) ([]NAPTR, error)
	// LookupSRV returns records sorted by priority and weight (RFC 2782),
	// as net.Resolver does.
	LookupSRV(ctx context.Context, service, proto, name string) ([]*net.SRV, error)
	LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error)
}

// Request is what to resolve.
type Request struct {
	Host      string    // URI host: a name or an IP literal
	Port      int       // 0 if the URI has none
	Transport Transport // "" if neither the URI nor the account fixes it
	Secure    bool      // sips: URI or TLS required
	// Allowed transports, in preference order; empty means all three.
	Allowed []Transport
}

var naptrService = map[string]Transport{"SIP+D2U": UDP, "SIP+D2T": TCP, "SIPS+D2T": TLS}

func srvName(t Transport) (service, proto string) {
	switch t {
	case UDP:
		return "sip", "udp"
	case TCP:
		return "sip", "tcp"
	default:
		return "sips", "tcp"
	}
}

func defaultPort(t Transport) int {
	if t == TLS {
		return 5061
	}
	return 5060
}

// ErrNoTargets means nothing was found.
var ErrNoTargets = errors.New("resolve: no SIP server found")

// Resolve returns the targets for r in the order to try them.
func Resolve(ctx context.Context, dns DNS, r Request) ([]Target, error) {
	allowed := r.Allowed
	if len(allowed) == 0 {
		allowed = []Transport{UDP, TCP, TLS}
	}
	if r.Secure {
		allowed = []Transport{TLS}
	}
	ok := func(t Transport) bool {
		for _, a := range allowed {
			if a == t {
				return true
			}
		}
		return false
	}
	if r.Transport != "" && !ok(r.Transport) {
		return nil, fmt.Errorf("resolve: transport %s not allowed here", r.Transport)
	}
	host := strings.TrimSuffix(strings.Trim(r.Host, "[]"), ".")
	if host == "" {
		return nil, errors.New("resolve: empty host")
	}

	// §4.1 transport, §4.2 port, for a numeric host or an explicit port.
	if ip, err := netip.ParseAddr(host); err == nil || r.Port != 0 {
		t := r.Transport
		if t == "" {
			t = allowed[0]
			if r.Secure {
				t = TLS
			} else if ok(UDP) {
				t = UDP
			}
		}
		port := r.Port
		if port == 0 {
			port = defaultPort(t)
		}
		if err == nil {
			return []Target{{Transport: t, Addr: netip.AddrPortFrom(ip.Unmap(), uint16(port)), Host: host}}, nil
		}
		return addrTargets(ctx, dns, host, port, t)
	}

	// Transport fixed: SRV for it, else A/AAAA on the default port.
	if r.Transport != "" {
		if ts, err := srvTargets(ctx, dns, host, r.Transport); err == nil && len(ts) > 0 {
			return ts, nil
		}
		return addrTargets(ctx, dns, host, defaultPort(r.Transport), r.Transport)
	}

	// NAPTR (§4.1): services SIP+D2U / SIP+D2T / SIPS+D2T with flag "s".
	if recs, err := dns.LookupNAPTR(ctx, host); err == nil && len(recs) > 0 {
		sort.SliceStable(recs, func(i, j int) bool {
			if recs[i].Order != recs[j].Order {
				return recs[i].Order < recs[j].Order
			}
			return recs[i].Preference < recs[j].Preference
		})
		var out []Target
		for _, n := range recs {
			t, known := naptrService[strings.ToUpper(n.Service)]
			if !known || !ok(t) || !strings.EqualFold(n.Flags, "s") || n.Replacement == "" {
				continue
			}
			srvs, err := dns.LookupSRV(ctx, "", "", strings.TrimSuffix(n.Replacement, "."))
			if err != nil {
				continue
			}
			out = append(out, srvToTargets(ctx, dns, srvs, t)...)
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	// No usable NAPTR: SRV for each allowed transport, in preference order.
	var out []Target
	for _, t := range allowed {
		if ts, err := srvTargets(ctx, dns, host, t); err == nil {
			out = append(out, ts...)
		}
	}
	if len(out) > 0 {
		return out, nil
	}

	// Finally A/AAAA: UDP for sip:, TLS for sips: (§4.2).
	t := UDP
	if r.Secure || !ok(UDP) {
		t = allowed[0]
	}
	return addrTargets(ctx, dns, host, defaultPort(t), t)
}

func srvTargets(ctx context.Context, dns DNS, host string, t Transport) ([]Target, error) {
	svc, proto := srvName(t)
	srvs, err := dns.LookupSRV(ctx, svc, proto, host)
	if err != nil {
		return nil, err
	}
	return srvToTargets(ctx, dns, srvs, t), nil
}

func srvToTargets(ctx context.Context, dns DNS, srvs []*net.SRV, t Transport) []Target {
	var out []Target
	for _, s := range srvs {
		target := strings.TrimSuffix(s.Target, ".")
		if target == "" { // "." means: service not available here
			continue
		}
		ts, err := addrTargets(ctx, dns, target, int(s.Port), t)
		if err == nil {
			out = append(out, ts...)
		}
	}
	return out
}

func addrTargets(ctx context.Context, dns DNS, host string, port int, t Transport) ([]Target, error) {
	ips, err := dns.LookupNetIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve: %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: %s has no address", ErrNoTargets, host)
	}
	// IPv4 first: many providers announce AAAA records for IPv6 that is less
	// well tested than their IPv4.
	sort.SliceStable(ips, func(i, j int) bool { return ips[i].Unmap().Is4() && !ips[j].Unmap().Is4() })
	out := make([]Target, len(ips))
	for i, ip := range ips {
		out[i] = Target{Transport: t, Addr: netip.AddrPortFrom(ip.Unmap(), uint16(port)), Host: host}
	}
	return out, nil
}

// ParseHostPort splits "host[:port]" as accounts store registrar and proxy.
func ParseHostPort(s string) (host string, port int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0, errors.New("resolve: empty address")
	}
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("resolve: bad port in %q", s)
		}
		return h, n, nil
	}
	return strings.Trim(s, "[]"), 0, nil
}
