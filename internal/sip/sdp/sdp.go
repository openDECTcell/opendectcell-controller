// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sdp builds and answers SDP offers for audio calls (RFC 3264):
// codec choice, RFC 4733 telephone-event, SRTP with SDES keys (RFC 4568)
// and hold directions (docs/sip-ua.md).
package sdp

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	psdp "github.com/pion/sdp/v3"
)

// Codec is an audio codec the controller can carry.
type Codec struct {
	Name  string // as in the account's codec list: G722, PCMA, PCMU
	PT    uint8  // static payload type
	Clock int    // RTP clock rate (8000 for G.722, RFC 3551 §4.5.2)
}

// Known codecs. Opus is listed in the API as "later" and not offered yet.
var Codecs = map[string]Codec{
	"G722": {"G722", 9, 8000},
	"PCMA": {"PCMA", 8, 8000},
	"PCMU": {"PCMU", 0, 8000},
}

// TelephoneEventPT is the dynamic payload type offered for RFC 4733.
const TelephoneEventPT = 101

// Direction is a media direction attribute.
type Direction string

const (
	SendRecv Direction = "sendrecv"
	SendOnly Direction = "sendonly"
	RecvOnly Direction = "recvonly"
	Inactive Direction = "inactive"
)

// Reverse is what the other side's direction means for us.
func (d Direction) Reverse() Direction {
	switch d {
	case SendOnly:
		return RecvOnly
	case RecvOnly:
		return SendOnly
	}
	return d
}

// Encryption is the account's media-encryption policy.
type Encryption string

const (
	EncOff       Encryption = "off"
	EncOptional  Encryption = "optional"
	EncMandatory Encryption = "mandatory"
)

// Suites we offer, strongest first (voip-requirements.md §7: the two
// AES_CM suites are must, GCM should).
var suites = []string{"AEAD_AES_128_GCM", "AES_CM_128_HMAC_SHA1_80", "AES_CM_128_HMAC_SHA1_32"}

// KeyLen is the master key‖salt length of a suite (RFC 4568, RFC 7714).
func KeyLen(suite string) int {
	switch suite {
	case "AES_CM_128_HMAC_SHA1_80", "AES_CM_128_HMAC_SHA1_32":
		return 30
	case "AEAD_AES_128_GCM":
		return 28
	}
	return 0
}

// Crypto is one a=crypto attribute.
type Crypto struct {
	Tag   int
	Suite string
	Key   []byte // master key ‖ salt
}

func (c Crypto) attr() string {
	return fmt.Sprintf("%d %s inline:%s", c.Tag, c.Suite, base64.StdEncoding.EncodeToString(c.Key))
}

func parseCrypto(v string) (Crypto, error) {
	f := strings.Fields(v)
	if len(f) < 3 {
		return Crypto{}, errors.New("sdp: short crypto attribute")
	}
	tag, err := strconv.Atoi(f[0])
	if err != nil || tag < 0 || tag > 999999999 {
		return Crypto{}, errors.New("sdp: bad crypto tag")
	}
	kp := f[2]
	if !strings.HasPrefix(kp, "inline:") {
		return Crypto{}, errors.New("sdp: only inline keys are supported")
	}
	kp = strings.TrimPrefix(kp, "inline:")
	if i := strings.IndexByte(kp, '|'); i >= 0 { // lifetime / MKI: not supported
		if strings.Count(kp[i:], "|") > 1 || strings.Contains(kp[i:], ":") {
			return Crypto{}, errors.New("sdp: MKI is not supported")
		}
		kp = kp[:i]
	}
	key, err := base64.StdEncoding.DecodeString(kp)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(kp)
	}
	if err != nil || len(key) != KeyLen(f[1]) {
		return Crypto{}, fmt.Errorf("sdp: bad key for %s", f[1])
	}
	return Crypto{Tag: tag, Suite: f[1], Key: key}, nil
}

// NewKey returns a random key for a suite.
func NewKey(suite string) ([]byte, error) {
	k := make([]byte, KeyLen(suite))
	if len(k) == 0 {
		return nil, fmt.Errorf("sdp: unknown suite %s", suite)
	}
	_, err := rand.Read(k)
	return k, err
}

// Local describes our media endpoint.
type Local struct {
	Addr       netip.AddrPort // RTP address advertised in c=/m=
	Codecs     []string       // the account's codec order
	Encryption Encryption
	Direction  Direction // "" = sendrecv
	PTime      int       // 0 = 20
	SessionID  uint64
	Version    uint64
}

// Media is the negotiated result for one side of a call.
type Media struct {
	Remote     netip.AddrPort // where to send RTP; port 0 = rejected
	Codec      Codec
	TelEventPT int // 0 if the peer has no telephone-event
	Direction  Direction
	RemoteKey  *Crypto // SRTP from the peer, nil if plain
	LocalKey   *Crypto // our SRTP key, nil if plain
	PTime      int
	OnHoldByUs bool // we sent sendonly/inactive
	HeldByPeer bool // the peer sent sendonly/inactive or c=0.0.0.0
}

// ErrNotAcceptable maps to 488 Not Acceptable Here.
var ErrNotAcceptable = errors.New("sdp: not acceptable")

func (l Local) direction() Direction {
	if l.Direction == "" {
		return SendRecv
	}
	return l.Direction
}

func (l Local) ptime() int {
	if l.PTime == 0 {
		return 20
	}
	return l.PTime
}

func newSession(l Local) *psdp.SessionDescription {
	nt := "IP4"
	if l.Addr.Addr().Is6() {
		nt = "IP6"
	}
	id := l.SessionID
	if id == 0 {
		var b [7]byte
		_, _ = rand.Read(b[:])
		for _, x := range b {
			id = id<<8 | uint64(x)
		}
	}
	return &psdp.SessionDescription{
		Version:     0,
		Origin:      psdp.Origin{Username: "opendectcell", SessionID: id, SessionVersion: l.Version, NetworkType: "IN", AddressType: nt, UnicastAddress: l.Addr.Addr().String()},
		SessionName: "opendectcell",
		ConnectionInformation: &psdp.ConnectionInformation{NetworkType: "IN", AddressType: nt,
			Address: &psdp.Address{Address: l.Addr.Addr().String()}},
		TimeDescriptions: []psdp.TimeDescription{{Timing: psdp.Timing{}}},
	}
}

func audioMedia(port uint16, proto string, pts []uint8, attrs []psdp.Attribute) *psdp.MediaDescription {
	formats := make([]string, len(pts))
	for i, pt := range pts {
		formats[i] = strconv.Itoa(int(pt))
	}
	return &psdp.MediaDescription{
		MediaName:  psdp.MediaName{Media: "audio", Port: psdp.RangedPort{Value: int(port)}, Protos: strings.Split(proto, "/"), Formats: formats},
		Attributes: attrs,
	}
}

// Offer builds an offer. With encryption it also returns our SRTP keys, one
// per offered suite, of which the answer picks one.
func Offer(l Local) ([]byte, []Crypto, error) {
	var pts []uint8
	var attrs []psdp.Attribute
	for _, name := range l.Codecs {
		c, ok := Codecs[name]
		if !ok {
			continue
		}
		pts = append(pts, c.PT)
		attrs = append(attrs, psdp.NewAttribute("rtpmap", fmt.Sprintf("%d %s/%d", c.PT, c.Name, c.Clock)))
	}
	if len(pts) == 0 {
		return nil, nil, errors.New("sdp: no usable codec in the account's list")
	}
	pts = append(pts, TelephoneEventPT)
	attrs = append(attrs,
		psdp.NewAttribute("rtpmap", fmt.Sprintf("%d telephone-event/8000", TelephoneEventPT)),
		psdp.NewAttribute("fmtp", fmt.Sprintf("%d 0-16", TelephoneEventPT)),
		psdp.NewAttribute("ptime", strconv.Itoa(l.ptime())),
		psdp.NewPropertyAttribute(string(l.direction())))
	proto := "RTP/AVP"
	var keys []Crypto
	if l.Encryption == EncOptional || l.Encryption == EncMandatory {
		if l.Encryption == EncMandatory {
			proto = "RTP/SAVP"
		}
		for i, s := range suites {
			k, err := NewKey(s)
			if err != nil {
				return nil, nil, err
			}
			c := Crypto{Tag: i + 1, Suite: s, Key: k}
			keys = append(keys, c)
			attrs = append(attrs, psdp.NewAttribute("crypto", c.attr()))
		}
	}
	sd := newSession(l)
	sd.MediaDescriptions = []*psdp.MediaDescription{audioMedia(l.Addr.Port(), proto, pts, attrs)}
	b, err := sd.Marshal()
	return b, keys, err
}

type remote struct {
	addr     netip.AddrPort
	proto    string
	pts      []int
	rtpmap   map[int]string // pt → "NAME/clock"
	cryptos  []Crypto
	dir      Direction
	ptime    int
	nullAddr bool
	mediaIdx int
}

func parse(b []byte) (*psdp.SessionDescription, *remote, error) {
	var sd psdp.SessionDescription
	if err := sd.UnmarshalString(string(b)); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrNotAcceptable, err)
	}
	for i, m := range sd.MediaDescriptions {
		if m.MediaName.Media != "audio" {
			continue
		}
		r := &remote{proto: strings.Join(m.MediaName.Protos, "/"), rtpmap: map[int]string{}, dir: SendRecv, mediaIdx: i}
		ci := m.ConnectionInformation
		if ci == nil {
			ci = sd.ConnectionInformation
		}
		if ci == nil || ci.Address == nil {
			return nil, nil, fmt.Errorf("%w: no connection address", ErrNotAcceptable)
		}
		a, err := netip.ParseAddr(ci.Address.Address)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: connection address %q", ErrNotAcceptable, ci.Address.Address)
		}
		r.nullAddr = a.IsUnspecified()
		r.addr = netip.AddrPortFrom(a, uint16(m.MediaName.Port.Value))
		for _, f := range m.MediaName.Formats {
			if pt, err := strconv.Atoi(f); err == nil {
				r.pts = append(r.pts, pt)
			}
		}
		dirSet := false
		for _, at := range m.Attributes {
			switch at.Key {
			case "rtpmap":
				if sp := strings.SplitN(at.Value, " ", 2); len(sp) == 2 {
					if pt, err := strconv.Atoi(sp[0]); err == nil {
						r.rtpmap[pt] = sp[1]
					}
				}
			case "crypto":
				if c, err := parseCrypto(at.Value); err == nil {
					r.cryptos = append(r.cryptos, c)
				}
			case "ptime":
				r.ptime, _ = strconv.Atoi(at.Value)
			case string(SendRecv), string(SendOnly), string(RecvOnly), string(Inactive):
				r.dir, dirSet = Direction(at.Key), true
			}
		}
		if !dirSet {
			for _, at := range sd.Attributes {
				switch at.Key {
				case string(SendRecv), string(SendOnly), string(RecvOnly), string(Inactive):
					r.dir = Direction(at.Key)
				}
			}
		}
		return &sd, r, nil
	}
	return nil, nil, fmt.Errorf("%w: no audio stream", ErrNotAcceptable)
}

// codecFor maps a payload type to a known codec, by rtpmap or static number.
func (r *remote) codecFor(pt int) (Codec, bool) {
	if m, ok := r.rtpmap[pt]; ok {
		name := strings.ToUpper(strings.SplitN(m, "/", 2)[0])
		c, known := Codecs[name]
		c.PT = uint8(pt)
		return c, known
	}
	for _, c := range Codecs {
		if int(c.PT) == pt && pt < 96 {
			return c, true
		}
	}
	return Codec{}, false
}

func (r *remote) telEvent() int {
	for _, pt := range r.pts {
		if m, ok := r.rtpmap[pt]; ok && strings.HasPrefix(strings.ToLower(m), "telephone-event/8000") {
			return pt
		}
	}
	return 0
}

// Answer answers a remote offer: the first of our codecs the offer has,
// telephone-event if offered, and SRTP per policy. It returns the answer
// body and the negotiated media.
func Answer(offer []byte, l Local) ([]byte, *Media, error) {
	sd, r, err := parse(offer)
	if err != nil {
		return nil, nil, err
	}
	secureOffer := strings.HasSuffix(r.proto, "SAVP")
	if r.proto != "RTP/AVP" && r.proto != "RTP/SAVP" {
		return nil, nil, fmt.Errorf("%w: transport %s", ErrNotAcceptable, r.proto)
	}
	m := &Media{Remote: r.addr, Direction: r.dir.Reverse(), PTime: r.ptime, HeldByPeer: r.dir == SendOnly || r.dir == Inactive || r.nullAddr}
	found := false
	for _, name := range l.Codecs {
		want, ok := Codecs[name]
		if !ok {
			continue
		}
		for _, pt := range r.pts {
			if c, ok := r.codecFor(pt); ok && c.Name == want.Name {
				m.Codec, found = c, true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("%w: no common codec", ErrNotAcceptable)
	}
	m.TelEventPT = r.telEvent()

	var local *Crypto
	switch {
	case l.Encryption == EncOff && secureOffer:
		return nil, nil, fmt.Errorf("%w: SRTP offered, encryption is off", ErrNotAcceptable)
	case l.Encryption != EncOff:
		for _, s := range suites {
			for _, c := range r.cryptos {
				if c.Suite == s {
					cc := c
					m.RemoteKey = &cc
					break
				}
			}
			if m.RemoteKey != nil {
				break
			}
		}
		if m.RemoteKey == nil && (l.Encryption == EncMandatory || secureOffer) {
			return nil, nil, fmt.Errorf("%w: no acceptable crypto suite", ErrNotAcceptable)
		}
		if m.RemoteKey != nil {
			k, err := NewKey(m.RemoteKey.Suite)
			if err != nil {
				return nil, nil, err
			}
			local = &Crypto{Tag: m.RemoteKey.Tag, Suite: m.RemoteKey.Suite, Key: k}
			m.LocalKey = local
		}
	}

	dir := l.direction()
	// Our direction must be compatible with theirs (RFC 3264 §6.1).
	switch r.dir {
	case SendOnly:
		if dir == SendRecv || dir == RecvOnly {
			dir = RecvOnly
		} else {
			dir = Inactive
		}
	case RecvOnly:
		if dir == SendRecv || dir == SendOnly {
			dir = SendOnly
		} else {
			dir = Inactive
		}
	case Inactive:
		dir = Inactive
	}
	m.Direction = dir
	m.OnHoldByUs = l.direction() == SendOnly || l.direction() == Inactive

	pts := []uint8{m.Codec.PT}
	attrs := []psdp.Attribute{psdp.NewAttribute("rtpmap", fmt.Sprintf("%d %s/%d", m.Codec.PT, m.Codec.Name, m.Codec.Clock))}
	if m.TelEventPT != 0 {
		pts = append(pts, uint8(m.TelEventPT))
		attrs = append(attrs, psdp.NewAttribute("rtpmap", fmt.Sprintf("%d telephone-event/8000", m.TelEventPT)),
			psdp.NewAttribute("fmtp", fmt.Sprintf("%d 0-16", m.TelEventPT)))
	}
	ptime := l.ptime()
	if r.ptime == 10 || r.ptime == 20 || r.ptime == 30 || r.ptime == 40 {
		ptime = r.ptime
	}
	m.PTime = ptime
	attrs = append(attrs, psdp.NewAttribute("ptime", strconv.Itoa(ptime)), psdp.NewPropertyAttribute(string(dir)))
	if local != nil {
		attrs = append(attrs, psdp.NewAttribute("crypto", local.attr()))
	}
	ans := newSession(l)
	ans.MediaDescriptions = make([]*psdp.MediaDescription, len(sd.MediaDescriptions))
	for i, md := range sd.MediaDescriptions {
		if i == r.mediaIdx {
			ans.MediaDescriptions[i] = audioMedia(l.Addr.Port(), r.proto, pts, attrs)
			continue
		}
		// Reject other streams with port 0 (RFC 3264 §6).
		ans.MediaDescriptions[i] = &psdp.MediaDescription{MediaName: psdp.MediaName{Media: md.MediaName.Media, Port: psdp.RangedPort{Value: 0},
			Protos: md.MediaName.Protos, Formats: md.MediaName.Formats}}
	}
	b, err := ans.Marshal()
	return b, m, err
}

// ParseAnswer reads the answer to an offer we made with offeredKeys.
func ParseAnswer(answer []byte, l Local, offeredKeys []Crypto) (*Media, error) {
	_, r, err := parse(answer)
	if err != nil {
		return nil, err
	}
	m := &Media{Remote: r.addr, Direction: r.dir.Reverse(), PTime: r.ptime, HeldByPeer: r.dir == SendOnly || r.dir == Inactive || r.nullAddr,
		OnHoldByUs: l.direction() == SendOnly || l.direction() == Inactive}
	if r.addr.Port() == 0 {
		return nil, fmt.Errorf("%w: audio rejected", ErrNotAcceptable)
	}
	for _, pt := range r.pts {
		if c, ok := r.codecFor(pt); ok {
			m.Codec = c
			break
		}
	}
	if m.Codec.Name == "" {
		return nil, fmt.Errorf("%w: answer has no codec we offered", ErrNotAcceptable)
	}
	m.TelEventPT = r.telEvent()
	if len(r.cryptos) > 0 {
		rc := r.cryptos[0]
		for _, k := range offeredKeys {
			if k.Tag == rc.Tag && k.Suite == rc.Suite {
				kk := k
				m.LocalKey, m.RemoteKey = &kk, &rc
			}
		}
		if m.LocalKey == nil {
			return nil, fmt.Errorf("%w: answer's crypto matches nothing offered", ErrNotAcceptable)
		}
	}
	if l.Encryption == EncMandatory && m.RemoteKey == nil {
		return nil, fmt.Errorf("%w: encryption is mandatory, the answer is plain", ErrNotAcceptable)
	}
	return m, nil
}
