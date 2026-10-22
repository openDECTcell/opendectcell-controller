// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
)

// When Start returns, the UDP listener must be in sipgo's connection pool.
// Otherwise the first requests make sipgo open a second socket on the same
// address, and accounts registering at start-up race it: in the interop run
// with two accounts, one never sent its REGISTER.
func TestStartRegistersTheListenerFirst(t *testing.T) {
	for i := 0; i < 20; i++ {
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		m, err := New(Config{UDP: conn, RTP: &rtp.Ports{IP: netip.MustParseAddr("127.0.0.1"), Min: 44000, Max: 44099}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		m.Start(context.Background())
		c, err := m.ua.TransportLayer().GetConnection("udp", conn.LocalAddr().String())
		m.Close()
		if err != nil || c == nil {
			t.Fatalf("run %d: listener not in the pool when Start returned (%v)", i, err)
		}
	}
}
