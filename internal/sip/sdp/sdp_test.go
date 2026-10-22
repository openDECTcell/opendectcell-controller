// SPDX-License-Identifier: AGPL-3.0-or-later

package sdp

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

var ctl = Local{Addr: netip.MustParseAddrPort("192.0.2.10:40002"), Codecs: []string{"G722", "PCMA", "PCMU"}, Encryption: EncOff}

// A typical Asterisk offer (PCMU first, G.722 later, telephone-event 101).
const asteriskOffer = "v=0\r\no=- 1234 1 IN IP4 198.51.100.20\r\ns=Asterisk\r\nc=IN IP4 198.51.100.20\r\nt=0 0\r\n" +
	"m=audio 17000 RTP/AVP 0 8 9 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:9 G722/8000\r\n" +
	"a=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-16\r\na=ptime:20\r\na=maxptime:150\r\na=sendrecv\r\n"

func TestAnswerAsterisk(t *testing.T) {
	ans, m, err := Answer([]byte(asteriskOffer), ctl)
	if err != nil {
		t.Fatal(err)
	}
	if m.Codec.Name != "G722" || m.Codec.PT != 9 || m.TelEventPT != 101 || m.Remote.String() != "198.51.100.20:17000" || m.Direction != SendRecv {
		t.Fatalf("media: %+v", m)
	}
	s := string(ans)
	for _, want := range []string{"m=audio 40002 RTP/AVP 9 101", "a=rtpmap:9 G722/8000", "c=IN IP4 192.0.2.10", "a=sendrecv", "a=ptime:20"} {
		if !strings.Contains(s, want) {
			t.Errorf("answer lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "crypto") || strings.Contains(s, "PCMU") {
		t.Errorf("answer has extra content:\n%s", s)
	}
}

func TestOfferAnswerRoundTripAndSRTP(t *testing.T) {
	for _, pol := range []Encryption{EncOff, EncOptional, EncMandatory} {
		a := ctl
		a.Encryption = pol
		offer, keys, err := Offer(a)
		if err != nil {
			t.Fatal(err)
		}
		s := string(offer)
		wantProto := map[Encryption]string{EncOff: "RTP/AVP", EncOptional: "RTP/AVP", EncMandatory: "RTP/SAVP"}[pol]
		if !strings.Contains(s, "m=audio 40002 "+wantProto+" 9 8 0 101") {
			t.Fatalf("%s offer:\n%s", pol, s)
		}
		if (pol == EncOff) != (len(keys) == 0) || strings.Count(s, "a=crypto:") != len(keys) {
			t.Fatalf("%s: %d keys, %d crypto lines", pol, len(keys), strings.Count(s, "a=crypto:"))
		}
		// The peer answers with the same policy.
		b := Local{Addr: netip.MustParseAddrPort("198.51.100.5:30000"), Codecs: []string{"PCMA", "G722"}, Encryption: pol}
		ans, bm, err := Answer(offer, b)
		if err != nil {
			t.Fatalf("%s answer: %v", pol, err)
		}
		if bm.Codec.Name != "PCMA" {
			t.Errorf("%s: answerer should prefer its own order, got %s", pol, bm.Codec.Name)
		}
		am, err := ParseAnswer(ans, a, keys)
		if err != nil {
			t.Fatalf("%s parse answer: %v", pol, err)
		}
		if am.Codec.Name != "PCMA" || am.Remote.String() != "198.51.100.5:30000" {
			t.Errorf("%s: %+v", pol, am)
		}
		if pol != EncOff {
			if am.LocalKey == nil || am.RemoteKey == nil || am.LocalKey.Suite != "AEAD_AES_128_GCM" {
				t.Fatalf("%s keys: %+v %+v", pol, am.LocalKey, am.RemoteKey)
			}
			// Each side's send key is the other's receive key.
			if string(am.RemoteKey.Key) != string(bm.LocalKey.Key) || string(bm.RemoteKey.Key) != string(am.LocalKey.Key) {
				t.Errorf("%s: keys do not cross", pol)
			}
		}
	}
}

func TestPolicyMismatch(t *testing.T) {
	mand := ctl
	mand.Encryption = EncMandatory
	if _, _, err := Answer([]byte(asteriskOffer), mand); !errors.Is(err, ErrNotAcceptable) {
		t.Errorf("mandatory, plain offer: %v", err)
	}
	savp, _, _ := Offer(mand)
	if _, _, err := Answer(savp, ctl); !errors.Is(err, ErrNotAcceptable) {
		t.Errorf("off, SAVP offer: %v", err)
	}
	// Optional answers a plain offer in plain.
	opt := ctl
	opt.Encryption = EncOptional
	if _, m, err := Answer([]byte(asteriskOffer), opt); err != nil || m.LocalKey != nil {
		t.Errorf("optional, plain offer: %+v %v", m, err)
	}
	// No common codec.
	onlyU := Local{Addr: ctl.Addr, Codecs: []string{"PCMU"}}
	g722 := strings.Replace(asteriskOffer, "RTP/AVP 0 8 9 101", "RTP/AVP 9 101", 1)
	if _, _, err := Answer([]byte(g722), onlyU); !errors.Is(err, ErrNotAcceptable) {
		t.Errorf("no common codec: %v", err)
	}
	// A mandatory offerer refuses a plain answer.
	_, keys, _ := Offer(mand)
	if _, err := ParseAnswer([]byte(asteriskOffer), mand, keys); !errors.Is(err, ErrNotAcceptable) {
		t.Errorf("mandatory, plain answer: %v", err)
	}
}

func TestHold(t *testing.T) {
	held := strings.Replace(asteriskOffer, "a=sendrecv", "a=sendonly", 1)
	ans, m, err := Answer([]byte(held), ctl)
	if err != nil || !m.HeldByPeer || m.Direction != RecvOnly || !strings.Contains(string(ans), "a=recvonly") {
		t.Fatalf("remote hold: %+v %v", m, err)
	}
	old := strings.Replace(asteriskOffer, "c=IN IP4 198.51.100.20", "c=IN IP4 0.0.0.0", 1)
	if _, m, _ := Answer([]byte(old), ctl); !m.HeldByPeer {
		t.Error("c=0.0.0.0 hold not recognised")
	}
	// We put the call on hold: our re-offer says sendonly.
	h := ctl
	h.Direction = SendOnly
	offer, _, _ := Offer(h)
	if !strings.Contains(string(offer), "a=sendonly") {
		t.Fatalf("hold offer:\n%s", offer)
	}
	_, pm, err := Answer(offer, Local{Addr: netip.MustParseAddrPort("198.51.100.5:30000"), Codecs: []string{"G722"}})
	if err != nil || pm.Direction != RecvOnly {
		t.Fatalf("peer's view of our hold: %+v %v", pm, err)
	}
}

func TestOtherStreamsRejectedAndCryptoParsing(t *testing.T) {
	withVideo := asteriskOffer + "m=video 17002 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n"
	ans, _, err := Answer([]byte(withVideo), ctl)
	if err != nil || !strings.Contains(string(ans), "m=video 0 RTP/AVP 96") {
		t.Fatalf("video not rejected:\n%s %v", ans, err)
	}
	for _, bad := range []string{"1 AES_CM_128_HMAC_SHA1_80 inline:AAAA", "x AES_CM_128_HMAC_SHA1_80 inline:", "1 AES_CM_128_HMAC_SHA1_80 uri:x",
		"1 AES_CM_128_HMAC_SHA1_80 inline:MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkw|2^20|1:4"} {
		if _, err := parseCrypto(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if c, err := parseCrypto("1 AES_CM_128_HMAC_SHA1_80 inline:MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkw|2^31"); err != nil || len(c.Key) != 30 {
		t.Errorf("lifetime suffix: %+v %v", c, err)
	}
}
