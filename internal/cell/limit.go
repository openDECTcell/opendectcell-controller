// SPDX-License-Identifier: AGPL-3.0-or-later

package cell

import (
	"net/netip"
	"sync"
)

// Limits on connections that have not proved who they are. A connection counts
// from accept until it is authenticated (a client certificate that checks out)
// or has finished its enrollment, so a flood of half-open or idle connections
// cannot use up file descriptors, goroutines and TLS work, and the ones that
// are authenticated no longer count. The numbers are far above a real site
// (a cell holds one connection; enrolment is rare).
const (
	DefaultMaxPreAuth        = 512
	DefaultMaxPreAuthPerAddr = 16
)

// preAuth counts the unauthenticated connections, in all and per source. A
// source is an IPv4 address or an IPv6 /64.
type preAuth struct {
	mu      sync.Mutex
	maxAll  int
	maxAddr int
	all     int
	by      map[netip.Addr]int
}

func newPreAuth(maxAll, maxAddr int) *preAuth {
	if maxAll <= 0 {
		maxAll = DefaultMaxPreAuth
	}
	if maxAddr <= 0 {
		maxAddr = DefaultMaxPreAuthPerAddr
	}
	return &preAuth{maxAll: maxAll, maxAddr: maxAddr, by: map[netip.Addr]int{}}
}

func preAuthKey(a netip.Addr) netip.Addr {
	a = a.Unmap()
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.Addr()
		}
	}
	return a
}

// acquire takes a place for a new connection from addr. The returned function
// gives it back; it is safe to call more than once. False: over a limit.
func (p *preAuth) acquire(addr netip.Addr) (release func(), ok bool) {
	k := preAuthKey(addr)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.all >= p.maxAll || p.by[k] >= p.maxAddr {
		return nil, false
	}
	p.all++
	p.by[k]++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.all--
			if p.by[k]--; p.by[k] <= 0 {
				delete(p.by, k)
			}
		})
	}, true
}

// PreAuthCount is the number of connections that are not authenticated yet, for metrics
// and tests.
func (s *Server) PreAuthCount() int {
	s.mu.Lock()
	p := s.pre
	s.mu.Unlock()
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.all
}
