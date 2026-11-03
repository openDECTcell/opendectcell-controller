// SPDX-License-Identifier: AGPL-3.0-or-later

package cell

import (
	"net/netip"
	"testing"
)

func TestPreAuthLimits(t *testing.T) {
	p := newPreAuth(5, 2)
	a := netip.MustParseAddr("192.0.2.1")
	b := netip.MustParseAddr("192.0.2.2")
	r1, ok1 := p.acquire(a)
	r2, ok2 := p.acquire(a)
	if !ok1 || !ok2 {
		t.Fatal("within the per-source limit")
	}
	if _, ok := p.acquire(a); ok {
		t.Fatal("per-source limit not enforced")
	}
	// Another source is not affected.
	if _, ok := p.acquire(b); !ok {
		t.Fatal("a second source was refused")
	}
	r1()
	r1() // giving a place back twice counts once
	if _, ok := p.acquire(a); !ok {
		t.Fatal("a released place was not free")
	}
	if _, ok := p.acquire(a); ok {
		t.Fatal("double release freed two places")
	}
	_ = r2

	// The global limit.
	q := newPreAuth(3, 10)
	for i := range 3 {
		if _, ok := q.acquire(netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})); !ok {
			t.Fatalf("source %d refused below the global limit", i)
		}
	}
	if _, ok := q.acquire(netip.MustParseAddr("198.51.100.200")); ok {
		t.Fatal("global limit not enforced")
	}
}

// An IPv6 source is one /64, so a client with a whole prefix gets no more
// places than a single IPv4 address.
func TestPreAuthIPv6PrefixIsOneSource(t *testing.T) {
	p := newPreAuth(100, 2)
	for _, s := range []string{"2001:db8:1:2::1", "2001:db8:1:2:ffff::7"} {
		if _, ok := p.acquire(netip.MustParseAddr(s)); !ok {
			t.Fatal(s)
		}
	}
	if _, ok := p.acquire(netip.MustParseAddr("2001:db8:1:2::99")); ok {
		t.Fatal("a third address of one /64 was accepted")
	}
	if _, ok := p.acquire(netip.MustParseAddr("2001:db8:1:3::1")); !ok {
		t.Fatal("another /64 was refused")
	}
	// An IPv4-mapped address is the IPv4 one.
	if preAuthKey(netip.MustParseAddr("::ffff:192.0.2.7")) != netip.MustParseAddr("192.0.2.7") {
		t.Fatal("IPv4-mapped address not unmapped")
	}
}
