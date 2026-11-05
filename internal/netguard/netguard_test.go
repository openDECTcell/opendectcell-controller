// SPDX-License-Identifier: AGPL-3.0-or-later

package netguard

import (
	"errors"
	"testing"
)

func TestControl(t *testing.T) {
	for _, c := range []struct {
		addr    string
		p       Policy
		blocked bool
	}{
		{"203.0.113.5:443", Policy{}, false},
		{"[2001:db8::1]:443", Policy{}, false},
		{"127.0.0.1:80", Policy{}, true},
		{"127.0.0.1:80", Policy{AllowPrivate: true}, true}, // private networks are not the machine itself
		{"[::1]:80", Policy{AllowPrivate: true}, true},
		{"127.0.0.1:80", Policy{AllowLoopback: true}, false},
		{"169.254.169.254:80", Policy{AllowPrivate: true}, true}, // the cloud metadata service
		{"[fe80::1]:80", Policy{AllowPrivate: true}, true},
		{"0.0.0.0:80", Policy{AllowPrivate: true}, true},
		{"224.0.0.1:80", Policy{AllowPrivate: true}, true},
		{"239.1.1.1:80", Policy{AllowPrivate: true}, true}, // multicast that is not link-local
		{"[ff05::1]:80", Policy{AllowPrivate: true}, true},
		{"10.1.2.3:80", Policy{}, true},
		{"192.168.1.9:80", Policy{}, true},
		{"172.16.0.1:80", Policy{}, true},
		{"[fd00::5]:80", Policy{}, true},
		{"10.1.2.3:80", Policy{AllowPrivate: true}, false},
		{"192.168.1.9:80", Policy{AllowPrivate: true}, false},
		{"[fd00::5]:80", Policy{AllowPrivate: true}, false},
		{"[::ffff:127.0.0.1]:80", Policy{AllowPrivate: true}, true}, // an IPv4 address written as IPv6 is the same address
		{"[::ffff:10.0.0.1]:80", Policy{}, true},
	} {
		err := c.p.Control("tcp", c.addr, nil)
		if (err != nil) != c.blocked || (err != nil && !errors.Is(err, ErrBlocked)) {
			t.Errorf("%s with %+v: %v, blocked want %v", c.addr, c.p, err, c.blocked)
		}
	}
	if err := (Policy{}).Control("tcp", "not an address", nil); err == nil {
		t.Error("an address that is not one was let through")
	}
	if err := (Policy{}).Control("tcp", "example.org:80", nil); err == nil {
		t.Error("a name was let through (the dialer resolves it first)")
	}
}
