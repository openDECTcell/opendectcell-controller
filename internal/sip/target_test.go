// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import (
	"net/netip"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
)

// A user-chosen server cannot be this host, a link-local address (a cloud's metadata service), a
// well-known port of another service, or a port below 1024 other than SIP's; a PBX on the LAN is fine.
func TestUsableTargets(t *testing.T) {
	m := &Manager{}
	for _, c := range []struct {
		addr string
		want bool
	}{
		{"192.0.2.10:5060", true},
		{"192.168.1.1:5060", true},
		{"10.0.0.5:5061", true},
		{"[2001:db8::1]:5060", true},
		{"203.0.113.9:49152", true},
		{"127.0.0.1:5060", false},
		{"[::1]:5060", false},
		{"0.0.0.0:5060", false},
		{"169.254.169.254:5060", false},
		{"224.0.1.75:5060", false},
		{"192.168.1.1:22", false},
		{"192.168.1.1:443", false},
		{"192.168.1.1:5432", false},
		{"192.168.1.1:6379", false},
		{"[::ffff:127.0.0.1]:5060", false},
	} {
		if got := m.usable(resolve.Target{Addr: netip.MustParseAddrPort(c.addr)}); got != c.want {
			t.Errorf("%s: %v, want %v", c.addr, got, c.want)
		}
	}
	local := &Manager{cfg: Config{AllowLocal: true}}
	if !local.usable(resolve.Target{Addr: netip.MustParseAddrPort("127.0.0.1:5060")}) {
		t.Error("AllowLocal did not allow loopback")
	}
	ts, err := m.filterTargets([]resolve.Target{{Addr: netip.MustParseAddrPort("127.0.0.1:5060")}, {Addr: netip.MustParseAddrPort("192.0.2.10:5060")}})
	if err != nil || len(ts) != 1 || ts[0].Addr.Addr().String() != "192.0.2.10" {
		t.Errorf("filtered %v %v", ts, err)
	}
	if _, err := m.filterTargets([]resolve.Target{{Addr: netip.MustParseAddrPort("127.0.0.1:5060")}}); err == nil {
		t.Error("a list with nothing usable was accepted")
	}
}
