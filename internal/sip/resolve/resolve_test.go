// SPDX-License-Identifier: AGPL-3.0-or-later

package resolve

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"
)

type fakeDNS struct {
	naptr map[string][]NAPTR
	srv   map[string][]*net.SRV // key: _svc._proto.name or the full name
	ips   map[string][]netip.Addr
}

func (f fakeDNS) LookupNAPTR(_ context.Context, name string) ([]NAPTR, error) {
	if r, ok := f.naptr[name]; ok {
		return r, nil
	}
	return nil, errors.New("NXDOMAIN")
}

func (f fakeDNS) LookupSRV(_ context.Context, svc, proto, name string) ([]*net.SRV, error) {
	key := name
	if svc != "" {
		key = "_" + svc + "._" + proto + "." + name
	}
	if r, ok := f.srv[key]; ok {
		return r, nil
	}
	return nil, errors.New("NXDOMAIN")
}

func (f fakeDNS) LookupNetIP(_ context.Context, host string) ([]netip.Addr, error) {
	if r, ok := f.ips[host]; ok {
		return r, nil
	}
	return nil, errors.New("NXDOMAIN")
}

var ip = netip.MustParseAddr

func strs(ts []Target) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.String())
	}
	return out
}

func TestResolve(t *testing.T) {
	dns := fakeDNS{
		naptr: map[string][]NAPTR{
			"provider.example": {
				{Order: 20, Preference: 10, Flags: "s", Service: "SIP+D2U", Replacement: "_sip._udp.provider.example."},
				{Order: 10, Preference: 20, Flags: "s", Service: "SIP+D2T", Replacement: "_sip._tcp.provider.example."},
				{Order: 10, Preference: 10, Flags: "S", Service: "SIPS+D2T", Replacement: "_sips._tcp.provider.example."},
				{Order: 5, Preference: 10, Flags: "s", Service: "SIP+D2W", Replacement: "_sip._ws.provider.example."},
			},
		},
		srv: map[string][]*net.SRV{
			"_sip._udp.provider.example":  {{Target: "a.provider.example.", Port: 5060}, {Target: "b.provider.example.", Port: 5062}},
			"_sip._tcp.provider.example":  {{Target: "a.provider.example.", Port: 5060}},
			"_sips._tcp.provider.example": {{Target: "a.provider.example.", Port: 5061}},
			"_sip._tcp.srvonly.example":   {{Target: "a.provider.example.", Port: 15060}},
			"_sip._udp.dot.example":       {{Target: ".", Port: 0}},
		},
		ips: map[string][]netip.Addr{
			"a.provider.example": {ip("2001:db8::1"), ip("192.0.2.1")},
			"b.provider.example": {ip("192.0.2.2")},
			"plain.example":      {ip("198.51.100.7")},
			"dot.example":        {ip("198.51.100.8")},
		},
	}
	for _, c := range []struct {
		name string
		req  Request
		want []string
	}{
		{"NAPTR order, preference, unknown service skipped", Request{Host: "provider.example"},
			[]string{"tls:192.0.2.1:5061", "tls:[2001:db8::1]:5061", "tcp:192.0.2.1:5060", "tcp:[2001:db8::1]:5060",
				"udp:192.0.2.1:5060", "udp:[2001:db8::1]:5060", "udp:192.0.2.2:5062"}},
		{"NAPTR, TLS not allowed", Request{Host: "provider.example", Allowed: []Transport{UDP, TCP}},
			[]string{"tcp:192.0.2.1:5060", "tcp:[2001:db8::1]:5060", "udp:192.0.2.1:5060", "udp:[2001:db8::1]:5060", "udp:192.0.2.2:5062"}},
		{"sips: only TLS", Request{Host: "provider.example", Secure: true}, []string{"tls:192.0.2.1:5061", "tls:[2001:db8::1]:5061"}},
		{"transport fixed: SRV for it", Request{Host: "provider.example", Transport: TCP}, []string{"tcp:192.0.2.1:5060", "tcp:[2001:db8::1]:5060"}},
		{"no NAPTR: SRV per transport", Request{Host: "srvonly.example"}, []string{"tcp:192.0.2.1:15060", "tcp:[2001:db8::1]:15060"}},
		{"no NAPTR, no SRV: A on 5060/UDP", Request{Host: "plain.example"}, []string{"udp:198.51.100.7:5060"}},
		{"no SRV, TLS fixed: A on 5061", Request{Host: "plain.example", Transport: TLS}, []string{"tls:198.51.100.7:5061"}},
		{"explicit port skips SRV", Request{Host: "provider.example", Port: 5080, Transport: UDP}, nil},
		{"explicit port, A record", Request{Host: "plain.example", Port: 5080}, []string{"udp:198.51.100.7:5080"}},
		{"IP literal", Request{Host: "203.0.113.5", Transport: TCP}, []string{"tcp:203.0.113.5:5060"}},
		{"IPv6 literal, sips", Request{Host: "[2001:db8::5]", Secure: true}, []string{"tls:[2001:db8::5]:5061"}},
		{"SRV '.' means not here; falls to A", Request{Host: "dot.example", Transport: UDP}, []string{"udp:198.51.100.8:5060"}},
	} {
		got, err := Resolve(context.Background(), dns, c.req)
		if c.want == nil {
			if err == nil {
				t.Errorf("%s: want error, got %v", c.name, strs(got))
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(strs(got), c.want) {
			t.Errorf("%s:\n got %v (%v)\nwant %v", c.name, strs(got), err, c.want)
		}
	}
	if _, err := Resolve(context.Background(), dns, Request{Host: "x", Transport: UDP, Allowed: []Transport{TLS}}); err == nil {
		t.Error("disallowed transport accepted")
	}
}

func TestParseHostPort(t *testing.T) {
	for in, want := range map[string]struct {
		h string
		p int
	}{"pbx.lan": {"pbx.lan", 0}, "pbx.lan:5080": {"pbx.lan", 5080}, "[2001:db8::1]:5061": {"2001:db8::1", 5061}, "[2001:db8::1]": {"2001:db8::1", 0}} {
		h, p, err := ParseHostPort(in)
		if err != nil || h != want.h || p != want.p {
			t.Errorf("%q: %s %d %v", in, h, p, err)
		}
	}
	for _, bad := range []string{"", "pbx:0", "pbx:99999", "pbx:x"} {
		if _, _, err := ParseHostPort(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
