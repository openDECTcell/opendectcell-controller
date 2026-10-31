// SPDX-License-Identifier: AGPL-3.0-or-later

package wire

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"reflect"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	for _, f := range []Frame{
		{Type: Heartbeat, Corr: 0, Body: HeartbeatBody(42)},
		{Type: StatusRequest, Corr: 7},
		{Type: Type(0x7777), Corr: 65535, Body: bytes.Repeat([]byte{9}, MaxBody)},
	} {
		b, err := f.Append(nil)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
	}
	for i := 0; i < 3; i++ {
		f, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if i == 0 {
			if v, _ := DecodeHeartbeat(f.Body); f.Type != Heartbeat || v != 42 {
				t.Errorf("heartbeat: %+v", f)
			}
		}
		if i == 2 && (len(f.Body) != MaxBody || f.Corr != 65535) {
			t.Errorf("max frame: %d %d", len(f.Body), f.Corr)
		}
	}
	if _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Errorf("end: %v", err)
	}
	if _, err := (Frame{Body: make([]byte, MaxBody+1)}).Append(nil); err == nil {
		t.Error("oversized body encoded")
	}
}

func TestFramingLost(t *testing.T) {
	for _, n := range []uint16{0, 3, 4 + MaxBody + 1, 0xFFFF} {
		b := []byte{byte(n >> 8), byte(n), 0, 3, 0, 0}
		if _, err := ReadFrame(bytes.NewReader(b)); !errors.Is(err, ErrFraming) {
			t.Errorf("length %d: %v", n, err)
		}
	}
}

func TestBounds(t *testing.T) {
	if err := (Frame{Type: Heartbeat, Body: make([]byte, 5)}).CheckBound(); err == nil {
		t.Error("5-byte heartbeat passed")
	}
	if err := (Frame{Type: StatusRequest, Body: []byte{1}}).CheckBound(); err == nil {
		t.Error("non-empty STATUS_REQUEST passed")
	}
	if err := (Frame{Type: Type(0x7777), Body: make([]byte, 100)}).CheckBound(); err != nil {
		t.Errorf("unknown type checked: %v", err)
	}
}

func TestTLV(t *testing.T) {
	b := new(Builder).U8(1, 5).String(2, "x").U8(1, 6).Bytes(9, nil).Build()
	tl, err := ParseTLV(b)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := tl.U8(1); v != 6 {
		t.Errorf("repeated tag: last value should win, got %d", v)
	}
	if len(tl.All(1)) != 2 || !tl.Has(9) || tl.Has(3) {
		t.Errorf("tags: %v", tl)
	}
	if _, err := tl.U16(1); !errors.Is(err, ErrMalformed) {
		t.Errorf("wrong length: %v", err)
	}
	if _, err := tl.U32(3); !errors.Is(err, ErrMalformed) {
		t.Errorf("missing: %v", err)
	}
	for _, bad := range [][]byte{{1}, {1, 0}, {1, 0, 5, 1, 2}} {
		if _, err := ParseTLV(bad); !errors.Is(err, ErrMalformed) {
			t.Errorf("%x: %v", bad, err)
		}
	}
}

func TestHelloRoundTrip(t *testing.T) {
	m := HelloMsg{Major: 1, Minor: 0, SWVersion: "dect-stubd 0.9", HWModel: "FRITZ!Box 7520",
		BootID: [16]byte{1, 2, 3}, ChipEpoch: 2, Capabilities: CapMail | CapMedia | CapSRTP | CapStats,
		CapSubRecords: 6, CapRadioContexts: 6, CapAudioNB: 4, CapAudioWB: 0, AudioFormats: []uint8{1}, AudioFormatsUp: []uint8{3, 1},
		ResumeToken: bytes.Repeat([]byte{7}, 16), RadioState: 1, CellTimeMs: 1761134400000}
	got, err := DecodeHello(m.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Errorf("got %+v\nwant %+v", got, m)
	}
	if _, err := DecodeHello(new(Builder).Bytes(0, nil).Build()); err == nil {
		t.Error("empty HELLO accepted")
	}
	bad := append(m.Encode(), 16, 0, 0) // tag 16, length 0
	if _, err := DecodeHello(bad); !errors.Is(err, ErrMalformed) {
		t.Errorf("empty audio_formats_up: %v", err)
	}
	m.AudioFormats = nil
	if _, err := DecodeHello(m.Encode()); !errors.Is(err, ErrMalformed) {
		t.Errorf("MEDIA without formats: %v", err)
	}
	if _, err := DecodeHello(new(Builder).Bytes(1, []byte{1, 0}).Build()); !errors.Is(err, ErrMalformed) {
		t.Errorf("missing required tags: %v", err)
	}
}

func TestOtherRoundTrips(t *testing.T) {
	ack := HelloAckMsg{Major: 1, CellID: [16]byte{9}, State: StateActive, SessionToken: [16]byte{4}, Resumed: true,
		HeartbeatMs: 1000, DeadMs: 3000, GraceMs: 7000, OrphanS: 60, Capabilities: CapMail, ControllerTimeMs: 5}
	if got, err := DecodeHelloAck(ack.Encode()); err != nil || got != ack {
		t.Errorf("HELLO_ACK: %+v %v", got, err)
	}
	er := EnrollRequestMsg{Token: bytes.Repeat([]byte{1}, TokenLen), CSR: []byte{0x30, 1, 2}, SWVersion: "v", NameHint: "box"}
	if got, err := DecodeEnrollRequest(er.Encode()); err != nil || !reflect.DeepEqual(got, er) {
		t.Errorf("ENROLL_REQUEST: %+v %v", got, err)
	}
	er.Token = er.Token[:19]
	if _, err := DecodeEnrollRequest(er.Encode()); err == nil {
		t.Error("19-byte token accepted")
	}
	resp := EnrollResponseMsg{Status: EnrollOK, CellID: [16]byte{3}, Cert: []byte{1}, CACert: []byte{2}, State: StatePending}
	if got, err := DecodeEnrollResponse(resp.Encode()); err != nil || !reflect.DeepEqual(got, resp) {
		t.Errorf("ENROLL_RESPONSE: %+v %v", got, err)
	}
	if got, err := DecodeEnrollResponse(EnrollResponseMsg{Status: EnrollTokenRejected}.Encode()); err != nil || got.Status != EnrollTokenRejected || got.Cert != nil {
		t.Errorf("rejected: %+v %v", got, err)
	}
	if got, err := DecodeGoodbye(GoodbyeMsg{ByeRevoked, "revoked by admin"}.Encode()); err != nil || got.Reason != ByeRevoked || got.Text != "revoked by admin" {
		t.Errorf("GOODBYE: %+v %v", got, err)
	}
	em := ErrorMsg{Code: ErrUnknownType, RefType: Type(0x7777), RefCorr: 3, Text: "unknown"}
	if got, err := DecodeError(em.Encode()); err != nil || got != em {
		t.Errorf("ERROR: %+v %v", got, err)
	}
	stats := map[uint16]uint64{1: 10, 19: 0, 999: 5}
	if got, err := DecodeStats(EncodeStats(stats)); err != nil || !reflect.DeepEqual(got, stats) {
		t.Errorf("STATS: %v %v", got, err)
	}
	if got, err := DecodeFlow(FlowMsg{2048, 1}.Encode()); err != nil || got != (FlowMsg{2048, 1}) {
		t.Errorf("FLOW: %+v %v", got, err)
	}
	st := StatusMsg{RadioState: 1, ChipState: 2, ChipEpoch: 3, UptimeS: 4, Detail: "ok", HasRadio: true, HasChip: true}
	if got, err := DecodeStatus(st.Encode()); err != nil || got != st {
		t.Errorf("STATUS: %+v %v", got, err)
	}
	if truncate("aé", 2) != "a" {
		t.Error("truncate split a rune")
	}
}

func FuzzReadFrameAndTLV(f *testing.F) {
	b, _ := Frame{Type: Hello, Corr: 1, Body: HelloMsg{Major: 1, SWVersion: "x"}.Encode()}.Append(nil)
	f.Add(b)
	f.Add([]byte{0, 4, 0, 1, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		for {
			fr, err := ReadFrame(r)
			if err != nil {
				return
			}
			_, _ = ParseTLV(fr.Body)
			_, _ = DecodeHello(fr.Body)
			_, _ = DecodeEnrollRequest(fr.Body)
			_, _ = DecodeStats(fr.Body)
		}
	})
}

func TestMediaOpenRoundTrip(t *testing.T) {
	for _, m := range []MediaOpenMsg{
		{MediaID: 1, AudioChannel: 1, Format: FormatL16_8k, PayloadType: 96, CtlAddr: netip.MustParseAddrPort("192.0.2.1:40000"),
			SSRCUp: 5, SSRCDown: 6, SRTPProfile: SRTPAES128SHA80, KeyUp: bytes.Repeat([]byte{1}, 30), KeyDown: bytes.Repeat([]byte{2}, 30)},
		{MediaID: 2, AudioChannel: 1, Format: FormatL16_8k, FormatUp: FormatPCMA, PtimeMs: 20, PayloadType: 96,
			CtlAddr: netip.MustParseAddrPort("[2001:db8::1]:40002"), SSRCUp: 7, SSRCDown: 8, SRTPProfile: SRTPNone, DSCP: 46},
	} {
		got, err := DecodeMediaOpen(m.Encode())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Errorf("got %+v\nwant %+v", got, m)
		}
	}
	m := MediaOpenMsg{Format: FormatL16_8k}
	if m.Uplink() != FormatL16_8k {
		t.Error("absent format_up is not the downlink format")
	}
	short := MediaOpenMsg{MediaID: 1, Format: 1, CtlAddr: netip.MustParseAddrPort("192.0.2.1:1"), SRTPProfile: SRTPAEAD128GCM, KeyUp: make([]byte, 30), KeyDown: make([]byte, 30)}
	if _, err := DecodeMediaOpen(short.Encode()); !errors.Is(err, ErrMalformed) {
		t.Errorf("GCM with 30-octet keys: %v", err)
	}
}

func TestMailCodec(t *testing.T) {
	up := MailUpMsg{RxTime: 0xA1B2C3D4, Src: 2, Dst: 0x14, Prim: 0x0203, Param: []byte{1, 2, 3, 4, 5}}
	b := up.Encode()
	if len(b) != 14 || b[6] != 0x02 || b[7] != 0x03 || b[8] != 5 {
		t.Fatalf("MAIL_UP layout: % x", b)
	}
	got, err := DecodeMailUp(b)
	if err != nil || got.RxTime != up.RxTime || got.Prim != up.Prim || string(got.Param) != string(up.Param) {
		t.Fatalf("MAIL_UP: %+v %v", got, err)
	}
	for _, bad := range [][]byte{b[:8], append(b, 0), {0, 0, 0, 0, 0, 0, 0, 0, 250}} {
		if _, err := DecodeMailUp(bad); !errors.Is(err, ErrMalformed) {
			t.Errorf("MAIL_UP % x accepted", bad)
		}
	}
	down := MailDownMsg{Src: 0x22, Dst: 2, Prim: 0x0244, Param: make([]byte, MailParamMax)}
	d, err := DecodeMailDown(down.Encode())
	if err != nil || d.Prim != 0x0244 || len(d.Param) != MailParamMax {
		t.Fatalf("MAIL_DOWN: %v", err)
	}
	r, err := DecodeMailReject(MailRejectMsg{RefCorr: 0x0102, Reason: RejectChipDown}.Encode())
	if err != nil || r.RefCorr != 0x0102 || r.Reason != RejectChipDown {
		t.Fatalf("MAIL_REJECT: %+v %v", r, err)
	}
}

func TestIdentityTags(t *testing.T) {
	rfpi := [5]byte{0x10, 0xf4, 0x20, 0x11, 0x01}
	h := HelloMsg{Major: 1, SWVersion: "x", CapSubRecords: 6, RFPI: rfpi, HasRFPI: true, AudioFormats: []uint8{1}}
	got, err := DecodeHello(h.Encode())
	if err != nil || !got.HasRFPI || got.RFPI != rfpi {
		t.Fatalf("hello: %+v %v", got, err)
	}
	got, err = DecodeHello(HelloMsg{Major: 1, SWVersion: "x", AudioFormats: []uint8{1}}.Encode())
	if err != nil || got.HasRFPI {
		t.Fatalf("hello without an RFPI: %+v %v", got, err)
	}
	s, err := DecodeStatus(StatusMsg{HasRadio: true, HasChip: true, RFPI: rfpi, HasRFPI: true}.Encode())
	if err != nil || !s.HasRFPI || s.RFPI != rfpi {
		t.Fatalf("status: %+v %v", s, err)
	}
	if _, err := DecodeStatus(new(Builder).Bytes(6, []byte{1, 2, 3}).Build()); err == nil {
		t.Error("a 3-octet RFPI accepted")
	}
	body := ConfigSetMsg{RFPI: rfpi, HasRFPI: true}.Encode()
	tl, err := ParseTLV(body)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := tl.Fixed(6, 5); err != nil || [5]byte(v) != rfpi {
		t.Fatalf("config set tag 6: %x %v", v, err)
	}
	if st, err := DecodeConfigAck(new(Builder).U16(1, 1).Build()); err != nil || st != 1 {
		t.Fatalf("config ack: %d %v", st, err)
	}
}
