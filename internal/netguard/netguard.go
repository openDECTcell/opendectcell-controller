// SPDX-License-Identifier: AGPL-3.0-or-later

// Package netguard stops the controller from being made to connect to its own surroundings: where a user gives an address that the
// controller then calls (an address book, a webhook), the dialer looks at every address after the name has been resolved, so that a name
// that resolves to the inside, and a redirect to it, are caught too.
package netguard

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
)

// ErrBlocked is the refusal.
var ErrBlocked = errors.New("that address is on this machine or its own network, which the controller does not connect to")

// Policy says which addresses may be connected to. Loopback, link-local (the cloud metadata service lives there), unspecified and
// multicast addresses are never allowed, except Loopback for tests.
type Policy struct {
	// AllowPrivate lets the controller reach private and unique-local addresses (10/8, 172.16/12, 192.168/16, fc00::/7).
	AllowPrivate bool
	// AllowLoopback is for tests that run a server on 127.0.0.1.
	AllowLoopback bool
}

// Control is a net.Dialer's Control function: the last look at an address before the connection is made.
func (p Policy) Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	ip = ip.Unmap()
	switch {
	case p.AllowLoopback && ip.IsLoopback():
		return nil
	case ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast():
		return ErrBlocked
	case ip.IsPrivate() && !p.AllowPrivate:
		return ErrBlocked
	}
	return nil
}
