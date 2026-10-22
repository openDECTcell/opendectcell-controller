// SPDX-License-Identifier: AGPL-3.0-or-later

package resolve

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// System uses Go's resolver for SRV and addresses (so /etc/hosts and
// container DNS work), and asks the servers in /etc/resolv.conf directly for
// NAPTR, which Go's resolver cannot look up.
type System struct {
	Resolver *net.Resolver // nil: net.DefaultResolver

	once    sync.Once
	servers []string
	cerr    error
}

func (s *System) resolver() *net.Resolver {
	if s.Resolver != nil {
		return s.Resolver
	}
	return net.DefaultResolver
}

func (s *System) LookupSRV(ctx context.Context, service, proto, name string) ([]*net.SRV, error) {
	_, srvs, err := s.resolver().LookupSRV(ctx, service, proto, name)
	return srvs, err
}

func (s *System) LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error) {
	return s.resolver().LookupNetIP(ctx, "ip", host)
}

func (s *System) LookupNAPTR(ctx context.Context, name string) ([]NAPTR, error) {
	s.once.Do(func() {
		cfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
		if err != nil {
			s.cerr = err
			return
		}
		for _, srv := range cfg.Servers {
			s.servers = append(s.servers, net.JoinHostPort(srv, cfg.Port))
		}
	})
	if s.cerr != nil {
		return nil, s.cerr
	}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeNAPTR)
	m.RecursionDesired = true
	c := &dns.Client{Timeout: 3 * time.Second}
	lastErr := errors.New("resolve: no DNS servers configured")
	for _, srv := range s.servers {
		r, _, err := c.ExchangeContext(ctx, m, srv)
		if err != nil {
			lastErr = err
			continue
		}
		if r.Truncated {
			c.Net = "tcp"
			if r, _, err = c.ExchangeContext(ctx, m, srv); err != nil {
				lastErr = err
				continue
			}
		}
		var out []NAPTR
		for _, rr := range r.Answer {
			if n, ok := rr.(*dns.NAPTR); ok {
				out = append(out, NAPTR{Order: n.Order, Preference: n.Preference, Flags: n.Flags, Service: n.Service, Replacement: n.Replacement})
			}
		}
		return out, nil
	}
	return nil, lastErr
}
