// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"bytes"
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/media/codec"
	"github.com/openDECTcell/opendectcell-controller/internal/media/dsp"
	"github.com/openDECTcell/opendectcell-controller/internal/media/quality"
)

// fakeLeg records what the pipeline sends.
type fakeLeg struct {
	mu   sync.Mutex
	sent []sentPkt
	ts   uint32
}

type sentPkt struct {
	pt      uint8
	payload []byte
	ts      uint32
	marker  bool
}

func (l *fakeLeg) ReadPacket(time.Time) (Packet, error) { select {} }
func (l *fakeLeg) WritePacket(pt uint8, payload []byte, units uint32, marker bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, sentPkt{pt, bytes.Clone(payload), l.ts, marker})
	l.ts += units
	return nil
}
func (l *fakeLeg) Skip(units uint32) { l.mu.Lock(); l.ts += units; l.mu.Unlock() }

// decoded is everything the leg was sent, decoded with c and placed by
// timestamp, so that skipped time is silence.
func (l *fakeLeg) decoded(c *codec.Codec) []int16 {
	l.mu.Lock()
	defer l.mu.Unlock()
	dec := c.NewDecoder()
	var out []int16
	for _, p := range l.sent {
		at := int(p.ts) * c.SampleRate / c.ClockRate
		for len(out) < at {
			out = append(out, 0)
		}
		out = dec.Decode(out, p.payload)
	}
	return out
}

// sim drives a pipeline on virtual time: a source signal is packetised in
// the input leg's codec every 20 ms, delivered with the given network
// behaviour, and the pipeline ticks every 10 ms.
type sim struct {
	t      *testing.T
	p      *Pipeline
	sip    *fakeLeg
	cell   *fakeLeg
	start  time.Time
	loss   float64       // probability a packet is lost
	jitter time.Duration // uniform extra delay, 0..jitter
	rng    *rand.Rand
	sipPT  uint8
	cellPT uint8
	plan   Plan
}

func newSim(t *testing.T, plan Plan, events EventSender) *sim {
	s := &sim{t: t, sip: &fakeLeg{}, cell: &fakeLeg{}, start: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC),
		rng: rand.New(rand.NewPCG(1, 2)), sipPT: 8, cellPT: 96, plan: plan} //nolint:gosec // reproducible network
	if plan.SIP == codec.G722 {
		s.sipPT = 9
	}
	var err error
	s.p, err = New(Config{Plan: plan, SIP: s.sip, SIPPayloadType: s.sipPT, Cell: s.cell, CellPayload: s.cellPT, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// run feeds src (at in's sample rate) from one leg and ticks the clock
// until everything has played out.
func (s *sim) run(fromCell bool, in *codec.Codec, src []int16) {
	type arrival struct {
		at  time.Time
		pkt Packet
	}
	var arr []arrival
	enc := in.NewEncoder()
	n := in.SampleRate / 50
	pt := s.sipPT
	if fromCell {
		pt = s.cellPT
	}
	for i := 0; i+n <= len(src); i += n {
		k := i / n
		payload := enc.Encode(nil, src[i:i+n])
		if s.rng.Float64() < s.loss {
			continue
		}
		at := s.start.Add(time.Duration(k)*20*time.Millisecond + time.Duration(s.rng.Int64N(int64(s.jitter)+1)))
		arr = append(arr, arrival{at, Packet{PT: pt, Seq: uint16(k), TS: uint32(k) * in.TimestampUnits(n), SSRC: 1, Marker: k == 0, Payload: payload}})
	}
	end := s.start.Add(time.Duration(len(src)/n)*20*time.Millisecond + 300*time.Millisecond)
	for now := s.start; now.Before(end); now = now.Add(tick) {
		for i := 0; i < len(arr); {
			if !arr[i].at.After(now) {
				s.p.Receive(fromCell, arr[i].pkt, arr[i].at)
				arr = append(arr[:i], arr[i+1:]...)
				continue
			}
			i++
		}
		s.p.Tick(now)
	}
}

// check compares what came out with the reference: the envelope must
// follow (the path carries our data) and, without loss, the waveform must
// match to minSNR after the path's delay.
func check(t *testing.T, name string, ref, got []int16, rate int, minSNR, minEnv float64) {
	t.Helper()
	if len(got) < len(ref)/2 {
		t.Fatalf("%s: %d samples out for %d in", name, len(got), len(ref))
	}
	env := quality.EnvelopeCorrelation(ref, got, rate, 15)
	snr, lag := quality.BestSNR(ref, got, rate/5) // up to 200 ms of delay
	t.Logf("%s: SNR %.1f dB at %.1f ms, envelope r = %.3f", name, snr, float64(lag)*1000/float64(rate), env)
	if env < minEnv {
		t.Errorf("%s: envelope correlation %.3f, want ≥ %.2f", name, env, minEnv)
	}
	if minSNR > 0 && snr < minSNR {
		t.Errorf("%s: SNR %.1f dB, want ≥ %.0f", name, snr, minSNR)
	}
}

var nbPlan = Plan{SIP: codec.PCMA, CellDown: wire.FormatL16_8k, CellUp: wire.FormatL16_8k, Rate: 8000}

// Today's call: PCMA on SIP, L16 8 kHz both ways on the cell.
func TestNarrowband(t *testing.T) {
	src := quality.SpeechLike(-20, 8000, 8000*4, 11)
	s := newSim(t, nbPlan, nil)
	s.run(false, codec.PCMA, src)
	// A-law once: the SNR is A-law's.
	check(t, "down", src, s.cell.decoded(codec.L16), 8000, 35, 0.98)
	s = newSim(t, nbPlan, nil)
	s.run(true, codec.L16, src)
	check(t, "up", src, s.sip.decoded(codec.PCMA), 8000, 35, 0.98)
}

// The case C5.4 is about: a G.722 SIP call on a narrowband-only cell is
// transcoded, through the 16 ↔ 8 kHz converters.
func TestG722OnNarrowbandCell(t *testing.T) {
	plan, err := Choose(narrowCell, true, codec.G722)
	if err != nil {
		t.Fatal(err)
	}
	src16 := quality.SpeechLike(-20, 16000, 16000*4, 12)
	var d dsp.Down2
	ref8 := d.Process(nil, src16) // the narrowband version of the source
	s := newSim(t, plan, nil)
	s.run(false, codec.G722, src16)
	check(t, "down", ref8, s.cell.decoded(codec.L16), 8000, 25, 0.97)

	src8 := quality.SpeechLike(-20, 8000, 8000*4, 13)
	var u dsp.Up2
	ref16 := u.Process(nil, src8)
	s = newSim(t, plan, nil)
	s.run(true, codec.L16, src8)
	check(t, "up", ref16, s.sip.decoded(codec.G722), 16000, 25, 0.97)
}

// If the cell's uplink turns out to be A-law, only the uplink format
// changes; the pipeline follows the plan.
func TestALawUplink(t *testing.T) {
	plan, err := Choose(CellCaps{Down: []uint8{wire.FormatL16_8k}, Up: []uint8{wire.FormatPCMA}}, false, codec.G722)
	if err != nil {
		t.Fatal(err)
	}
	src := quality.SpeechLike(-20, 8000, 8000*4, 14)
	s := newSim(t, plan, nil)
	s.run(true, codec.PCMA, src)
	var u dsp.Up2
	ref16 := u.Process(nil, src)
	check(t, "up, A-law from the cell", ref16, s.sip.decoded(codec.G722), 16000, 25, 0.97)
}

// Wideband both ways and G.722 on SIP: the octets are forwarded untouched.
func TestPassthroughIsBitExact(t *testing.T) {
	plan, _ := Choose(wideCell, true, codec.G722)
	if !plan.PassDown || !plan.PassUp {
		t.Fatalf("plan %v", plan)
	}
	src := quality.SpeechLike(-20, 16000, 16000*2, 15)
	payload := codec.G722.NewEncoder().Encode(nil, src)
	s := newSim(t, plan, nopEvents{})
	s.run(false, codec.G722, src)
	var got []byte
	for _, p := range s.cell.sent {
		got = append(got, p.payload...)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("passthrough changed the payload: %d octets out, %d in", len(got), len(payload))
	}
	if st := s.p.Stats(s.start); !st.Down.Passed || !st.Up.Passed {
		t.Errorf("stats do not show passthrough: %+v", st)
	}
}

type nopEvents struct{}

func (nopEvents) SendDTMF(context.Context, rune, time.Duration) error { return nil }

// Loss and jitter: the output keeps its pace, losses are concealed, and the
// talker's envelope comes through.
func TestLossAndJitter(t *testing.T) {
	src := quality.SpeechLike(-20, 8000, 8000*10, 16)
	s := newSim(t, nbPlan, nil)
	s.loss, s.jitter = 0.05, 40*time.Millisecond
	s.run(false, codec.PCMA, src)
	got := s.cell.decoded(codec.L16)
	check(t, "down, 5 % loss, 0–40 ms jitter", src, got, 8000, 0, 0.9)
	st := s.p.Stats(s.start.Add(11 * time.Second))
	if st.Down.Concealed < 15 || st.Down.Concealed > 60 {
		t.Errorf("%d frames concealed for ~25 lost packets", st.Down.Concealed)
	}
	// Paced: one packet every 20 ms, contiguous timestamps.
	for i := 1; i < len(s.cell.sent); i++ {
		if s.cell.sent[i].ts != s.cell.sent[i-1].ts+160 {
			t.Fatalf("output timestamps jump at packet %d", i)
		}
	}
	t.Logf("jitter buffer: %+v", st.Down.Jitter)
}

// Stateless passthrough (PCMA both ways) conceals a lost packet instead of
// leaving a hole.
func TestPassthroughConcealsG711(t *testing.T) {
	plan := Plan{SIP: codec.PCMA, CellDown: wire.FormatPCMA, CellUp: wire.FormatPCMA, Rate: 8000, PassDown: true, PassUp: true}
	src := quality.SpeechLike(-20, 8000, 8000*4, 17)
	s := newSim(t, plan, nopEvents{})
	s.loss = 0.05
	s.run(false, codec.PCMA, src)
	for i := 1; i < len(s.cell.sent); i++ {
		if s.cell.sent[i].ts != s.cell.sent[i-1].ts+160 {
			t.Fatalf("hole in the passthrough output at packet %d", i)
		}
	}
	check(t, "pass, 5 % loss", src, s.cell.decoded(codec.PCMA), 8000, 0, 0.9)
}

// Before anything arrives, the pipeline still sends paced silence, with
// the marker bit on the first packet only.
func TestSilenceBeforeMedia(t *testing.T) {
	s := newSim(t, nbPlan, nil)
	for i := 0; i < 50; i++ {
		s.p.Tick(s.start.Add(time.Duration(i) * tick))
	}
	if len(s.cell.sent) != 25 || len(s.sip.sent) != 25 {
		t.Fatalf("sent %d/%d packets in 500 ms, want 25 each", len(s.cell.sent), len(s.sip.sent))
	}
	if !s.cell.sent[0].marker || s.cell.sent[1].marker {
		t.Error("marker bit not on the first packet only")
	}
	if quality.RMS(s.cell.decoded(codec.L16)) != 0 {
		t.Error("not silence")
	}
	if s.sip.sent[0].pt != 8 || s.cell.sent[0].pt != 96 {
		t.Error("wrong payload types")
	}
}

func TestWrongPayloadType(t *testing.T) {
	s := newSim(t, nbPlan, nil)
	s.p.Receive(false, Packet{PT: 0, Payload: make([]byte, 160)}, s.start)
	if s.p.Stats(s.start).Down.WrongPT != 1 {
		t.Error("a PCMU packet on a PCMA call was not dropped")
	}
}

// Without telephone-event, DTMF goes into the uplink as tones.
func TestInbandDTMF(t *testing.T) {
	s := newSim(t, nbPlan, nil)
	if err := s.p.SendDTMF(context.Background(), '5', 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	s.run(true, codec.L16, quality.SpeechLike(-20, 8000, 8000, 18))
	out := s.sip.decoded(codec.PCMA)
	f, _ := dsp.DTMFFreqs('5')
	seg := out[:800] // the digit plays first, replacing the talker
	if lo := quality.DominantFrequency(seg, 8000, []float64{697, 770, 852, 941}); lo != f[0] {
		t.Errorf("low tone %v Hz, want %v", lo, f[0])
	}
	if hi := quality.DominantFrequency(seg, 8000, []float64{1209, 1336, 1477, 1633}); hi != f[1] {
		t.Errorf("high tone %v Hz, want %v", hi, f[1])
	}
	if err := s.p.SendDTMF(context.Background(), 'x', time.Second); err == nil {
		t.Error("accepted a non-digit")
	}
}

// With telephone-event, the digit goes to the SIP endpoint as an RFC 4733
// event, and uplink audio pauses while it plays.
func TestEventDTMF(t *testing.T) {
	ev := &blockingEvents{release: make(chan struct{}), called: make(chan rune, 1)}
	s := newSim(t, nbPlan, ev)
	done := make(chan error)
	go func() { done <- s.p.SendDTMF(context.Background(), '#', 100*time.Millisecond) }()
	if d := <-ev.called; d != '#' {
		t.Fatalf("event for %q", d)
	}
	for i := 0; i < 10; i++ {
		s.p.Tick(s.start.Add(time.Duration(i) * tick))
	}
	if len(s.sip.sent) != 0 {
		t.Errorf("%d audio packets sent during the event", len(s.sip.sent))
	}
	close(ev.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.p.Tick(s.start.Add(100 * time.Millisecond))
	s.p.Tick(s.start.Add(110 * time.Millisecond))
	if len(s.sip.sent) == 0 {
		t.Error("audio did not resume after the event")
	}
}

type blockingEvents struct {
	release chan struct{}
	called  chan rune
}

func (b *blockingEvents) SendDTMF(_ context.Context, d rune, _ time.Duration) error {
	b.called <- d
	<-b.release
	return nil
}

// Different ptimes on the two legs: 30 ms SIP packets against a 20 ms cell.
func TestPtimeMismatch(t *testing.T) {
	cell, sip := &fakeLeg{}, &fakeLeg{}
	p, err := New(Config{Plan: nbPlan, SIP: sip, SIPPayloadType: 8, SIPPtime: 30 * time.Millisecond, Cell: cell, CellPayload: 96})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 60; i++ {
		p.Tick(start.Add(time.Duration(i) * tick))
	}
	if len(cell.sent) != 30 || len(sip.sent) != 20 || len(sip.sent[0].payload) != 240 {
		t.Errorf("600 ms: %d cell packets (want 30), %d SIP packets of %d octets (want 20 of 240)", len(cell.sent), len(sip.sent), len(sip.sent[0].payload))
	}
	if _, err := New(Config{Plan: nbPlan, SIP: sip, SIPPtime: 25 * time.Millisecond, Cell: cell}); err == nil {
		t.Error("accepted a 25 ms ptime")
	}
}
