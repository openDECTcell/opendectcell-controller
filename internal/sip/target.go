// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import (
	"errors"
	"net/netip"

	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
)

// An account's registrar, proxy and domain are typed in by a user, and the controller connects to
// whatever they resolve to. Without a filter an account is a way to make the controller open
// connections to its own host, to services on its Docker network or to a cloud's metadata address,
// and to read the answer off the registration status. usable keeps the targets a SIP server could
// be: not this host, not a link-local address, and not a port that belongs to another kind of
// service. A PBX on the LAN (private addresses, 5060 or any other high port) stays allowed.

// deniedPorts are well-known ports of services a deployment may keep on a private network.
var deniedPorts = map[uint16]bool{
	2375: true, 2376: true, // Docker
	3306: true, 5432: true, 6379: true, 11211: true, 27017: true, // MySQL, PostgreSQL, Redis, memcached, MongoDB
	9200: true, 9300: true, // Elasticsearch
}

// errNoUsableTarget means every address the server name resolved to was filtered out.
var errNoUsableTarget = errors.New("the server address is not allowed (loopback, link-local or a port of another service)")

// usable says whether the controller may connect to t.
func (m *Manager) usable(t resolve.Target) bool {
	a := t.Addr.Addr().Unmap()
	p := t.Addr.Port()
	if !m.cfg.AllowLocal && (a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsUnspecified() || a.IsMulticast()) {
		return false
	}
	if p == 0 || deniedPorts[p] || (p < 1024 && p != 5060 && p != 5061) {
		return false
	}
	return a != netip.Addr{}
}

// filterTargets drops the targets that are not usable; an error if none is left.
func (m *Manager) filterTargets(ts []resolve.Target) ([]resolve.Target, error) {
	out := ts[:0:0]
	for _, t := range ts {
		if m.usable(t) {
			out = append(out, t)
		}
	}
	if len(out) == 0 && len(ts) > 0 {
		return nil, errNoUsableTarget
	}
	return out, nil
}
