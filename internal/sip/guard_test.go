// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import (
	"log/slog"
	"net/netip"
	"testing"

	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
)

// guardRig is a manager with one account, 330@pbx.test, whose server is at 192.0.2.10.
func guardRig() (*Manager, *line) {
	m := &Manager{log: slog.New(slog.DiscardHandler), lines: map[uuid.UUID]*line{}, byContact: map[string]*line{}}
	a := Account{ID: uuid.New(), Username: "330", Domain: "pbx.test", Enabled: true, Register: true}
	l := newLine(m, a)
	m.lines[a.ID], m.byContact[ContactUser(a.ID)] = l, l
	l.notePeers([]resolve.Target{{Addr: netip.MustParseAddrPort("192.0.2.10:5060")}})
	return m, l
}

// invite is an INVITE for the user in the Request-URI, To: the account's address, from source.
func invite(requestUser, source string) *sip.Request {
	req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: requestUser, Host: "controller.test"})
	req.AppendHeader(&sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: "330", Host: "pbx.test"}, Params: sip.NewParams()})
	req.SetSource(source)
	return req
}

// A request addressed by extension and domain is taken only from the account's own servers; one
// addressed by the account's secret Contact user part is taken from anywhere (that is the registrar's
// side, it was the only one told).
func TestIncomingNeedsTheContactTokenOrTheRegistrarsAddress(t *testing.T) {
	m, l := guardRig()
	token := ContactUser(l.acct.ID)
	for _, c := range []struct {
		name, user, source string
		want               bool
	}{
		{"by token from the server", token, "192.0.2.10:5060", true},
		{"by token from elsewhere (a provider's other address)", token, "198.51.100.7:5060", true},
		{"by extension from the server", "330", "192.0.2.10:5060", true},
		{"by extension from the server, a source port other than 5060", "330", "192.0.2.10:49152", true},
		{"by extension from a stranger", "330", "198.51.100.7:5060", false},
		{"by extension, an unparsable source", "330", "not an address", false},
	} {
		got, ok := m.accept(invite(c.user, c.source))
		if got != l || ok != c.want {
			t.Errorf("%s: line %v, accepted %v, want %v", c.name, got == l, ok, c.want)
		}
	}
	if got, _ := m.accept(invite("999", "192.0.2.10:5060")); got != nil && got != l {
		t.Error("another account was found")
	}
}

// The calls going at once are bounded per account and per source address, and a call that ended
// frees its place.
func TestIncomingCallsAreBounded(t *testing.T) {
	m, l := guardRig()
	src := netip.MustParseAddr("198.51.100.7")
	var releases []func()
	for range maxIncomingPerSource {
		r, ok := m.takeIncoming(l.acct.ID, src)
		if !ok {
			t.Fatal("refused under the limit")
		}
		releases = append(releases, r)
	}
	if _, ok := m.takeIncoming(l.acct.ID, src); ok {
		t.Error("one source went over its limit")
	}
	// other sources fill the account up to its own limit
	for i := range maxIncomingPerAccount - maxIncomingPerSource {
		r, ok := m.takeIncoming(l.acct.ID, netip.AddrFrom4([4]byte{203, 0, 113, byte(i + 1)}))
		if !ok {
			t.Fatal("refused under the account's limit")
		}
		releases = append(releases, r)
	}
	if _, ok := m.takeIncoming(l.acct.ID, netip.MustParseAddr("203.0.113.200")); ok {
		t.Error("an account went over its limit")
	}
	releases[0]()
	if _, ok := m.takeIncoming(l.acct.ID, src); !ok {
		t.Error("a finished call did not free its place")
	}
	for _, r := range releases[1:] {
		r()
	}
	if len(m.inAcct) != 1 || len(m.inSrc) != 1 { // the one taken again
		t.Errorf("counters left over: %v %v", m.inAcct, m.inSrc)
	}
}
