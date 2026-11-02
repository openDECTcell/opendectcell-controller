// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/media/codec"
	"github.com/openDECTcell/opendectcell-controller/internal/media/dsp"
	"github.com/openDECTcell/opendectcell-controller/internal/media/jitter"
)

// EventSender sends RFC 4733 telephone events on the SIP leg (the SIP media
// endpoint).
type EventSender interface {
	SendDTMF(ctx context.Context, digit rune, d time.Duration) error
}

// Config is one call's media.
type Config struct {
	Plan           Plan
	SIP            Leg
	SIPPayloadType uint8         // negotiated for Plan.SIP
	SIPPtime       time.Duration // negotiated; 0 means 20 ms
	Cell           Leg
	CellPayload    uint8         // MEDIA_OPEN tag 5, both directions
	CellPtime      time.Duration // MEDIA_OPEN tag 4; 0 means 20 ms
	// Events sends RFC 4733 events. Set it only when the SIP peer
	// negotiated telephone-event; without it, DTMF is generated in band.
	Events   EventSender
	MinDelay time.Duration // jitter buffer bounds; 0 for the defaults
	MaxDelay time.Duration
	Log      *slog.Logger
}

// tick is the pipeline's clock. Every ptime the media engine accepts
// (10, 20, 30, 40 ms) is a multiple of it.
const tick = 10 * time.Millisecond

// Pipeline carries one call's audio in both directions. Downlink is SIP →
// cell, uplink is cell → SIP. Each direction has its own jitter buffer,
// decoder, concealer, rate converters and encoder, and runs on the
// pipeline's clock: every ptime it produces exactly one frame for its output
// leg, from whatever the jitter buffer holds, so the output is paced
// whatever the input does.
type Pipeline struct {
	cfg        Config
	down, up   *direction
	eventBusy  atomic.Bool
	log        *slog.Logger
	readerDone sync.WaitGroup
}

// New builds a pipeline. Passthrough is used where the plan allows it and
// both legs use the same ptime; uplink passthrough also needs RFC 4733,
// because in-band DTMF has to be mixed into decoded audio.
func New(cfg Config) (*Pipeline, error) {
	if cfg.SIPPtime == 0 {
		cfg.SIPPtime = 20 * time.Millisecond
	}
	if cfg.CellPtime == 0 {
		cfg.CellPtime = 20 * time.Millisecond
	}
	for _, pt := range []time.Duration{cfg.SIPPtime, cfg.CellPtime} {
		if pt%tick != 0 || pt < tick || pt > 40*time.Millisecond {
			return nil, errors.New("media: ptime must be 10, 20, 30 or 40 ms")
		}
	}
	cellDown, cellUp := FormatCodec(cfg.Plan.CellDown), FormatCodec(cfg.Plan.CellUp)
	if cellDown == nil || cellUp == nil || cfg.Plan.SIP == nil {
		return nil, ErrNoFormat
	}
	samePtime := cfg.SIPPtime == cfg.CellPtime
	p := &Pipeline{cfg: cfg, log: cfg.Log}
	if p.log == nil {
		p.log = slog.Default()
	}
	p.down = newDirection("down", cfg, cfg.Plan.SIP, cfg.SIPPayloadType, cellDown, cfg.CellPayload, cfg.Cell, cfg.CellPtime,
		cfg.Plan.PassDown && samePtime)
	p.up = newDirection("up", cfg, cellUp, cfg.CellPayload, cfg.Plan.SIP, cfg.SIPPayloadType, cfg.SIP, cfg.SIPPtime,
		cfg.Plan.PassUp && samePtime && cfg.Events != nil)
	p.up.dtmf = dsp.NewDTMFGen(cfg.Plan.Rate)
	p.up.mute = p.eventBusy.Load
	return p, nil
}

type converter interface {
	Process(dst, in []int16) []int16
}

type identity struct{}

// Process appends in unchanged: source and sink run at the same sample rate.
func (identity) Process(dst, in []int16) []int16 { return append(dst, in...) }

func newConverter(from, to int) converter {
	switch from {
	case to:
		return identity{}
	case 2 * to:
		return new(dsp.Down2)
	default:
		return new(dsp.Up2)
	}
}

// direction is one way through the pipeline.
type direction struct {
	name     string
	in, out  *codec.Codec
	inPT     uint8
	outPT    uint8
	outLeg   Leg
	pass     bool
	ptime    time.Duration
	due      time.Duration
	frame    int    // internal samples per output frame
	outUnits uint32 // output timestamp units per frame

	mu    sync.Mutex // jb and dtmf: pushed and filled from other goroutines
	jb    *jitter.Buffer
	dtmf  *dsp.DTMFGen // uplink only
	stats DirStats

	dec    codec.Decoder
	enc    codec.Encoder
	plc    *dsp.Concealer // at the input rate
	toInt  converter
	toOut  converter
	fifo   []int16 // internal rate
	carry  uint32  // passthrough: input units in hand beyond the frames sent
	marker bool
	mute   func() bool

	pcm, frameBuf, outPCM []int16
	payload               []byte
}

// DirStats are one direction's counters.
type DirStats struct {
	Jitter    jitter.Stats
	Passed    bool   // passthrough
	Sent      uint64 // packets sent
	Concealed uint64 // frames (or packets, in passthrough) concealed or skipped
	Silence   uint64 // frames of silence before the first packet or in a gap
	WrongPT   uint64 // packets dropped for an unexpected payload type
	SendErrs  uint64
}

func newDirection(name string, cfg Config, in *codec.Codec, inPT uint8, out *codec.Codec, outPT uint8, outLeg Leg, ptime time.Duration, pass bool) *direction {
	rate := cfg.Plan.Rate
	d := &direction{name: name, in: in, out: out, inPT: inPT, outPT: outPT, outLeg: outLeg, pass: pass, ptime: ptime,
		frame:    rate * int(ptime/time.Millisecond) / 1000,
		outUnits: uint32(out.ClockRate * int(ptime/time.Millisecond) / 1000),
		dec:      in.NewDecoder(), enc: out.NewEncoder(), plc: dsp.NewConcealer(in.SampleRate),
		toInt: newConverter(in.SampleRate, rate), toOut: newConverter(rate, out.SampleRate), marker: true}
	d.stats.Passed = pass
	d.jb = jitter.New(jitter.Config{
		ClockRate: in.ClockRate,
		Frame:     uint32(in.ClockRate * int(ptime/time.Millisecond) / 1000),
		MinDelay:  cfg.MinDelay, MaxDelay: cfg.MaxDelay,
		Duration: func(p []byte) uint32 { return in.TimestampUnits(in.Samples(len(p))) },
	})
	return d
}

// push queues a received packet.
func (d *direction) push(p Packet, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p.PT != d.inPT {
		d.stats.WrongPT++
		return
	}
	d.jb.Push(jitter.Packet{Seq: p.Seq, TS: p.TS, Marker: p.Marker, SSRC: p.SSRC, Payload: p.Payload, Arrival: at})
}

func (d *direction) pop(now time.Time) (jitter.Packet, jitter.Status, uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.jb.Pop(now)
}

func (d *direction) samples(units uint32) int {
	return int(units) * d.in.SampleRate / d.in.ClockRate
}

// produce sends one frame.
func (d *direction) produce(now time.Time) {
	if d.pass {
		d.passOne(now)
		return
	}
	for len(d.fifo) < d.frame {
		pkt, st, n := d.pop(now)
		var pcm []int16
		switch st {
		case jitter.OK:
			pcm = d.dec.Decode(d.pcm[:0], pkt.Payload)
			d.plc.Good(pcm)
		case jitter.Lost:
			pcm = d.plc.Conceal(d.pcm[:0], d.samples(n))
			d.count(func(s *DirStats) { s.Concealed++ })
		default:
			d.pcm = append(d.pcm[:0], make([]int16, d.samples(n))...) // silence, in the reused buffer
			pcm = d.pcm
			d.plc.Good(pcm)
			d.count(func(s *DirStats) { s.Silence++ })
		}
		d.pcm = pcm
		d.fifo = d.toInt.Process(d.fifo, pcm)
	}
	d.frameBuf = append(d.frameBuf[:0], d.fifo[:d.frame]...)
	frame := d.frameBuf
	d.fifo = d.fifo[:copy(d.fifo, d.fifo[d.frame:])]
	if d.dtmf != nil {
		d.mu.Lock()
		d.dtmf.Fill(frame)
		d.mu.Unlock()
	}
	if d.mute != nil && d.mute() {
		return // an RFC 4733 event owns the timestamps
	}
	d.outPCM = d.toOut.Process(d.outPCM[:0], frame)
	d.payload = d.enc.Encode(d.payload[:0], d.outPCM)
	d.send(d.payload, d.out.TimestampUnits(len(d.outPCM)))
}

// passOne forwards packets until a frame's worth of time has passed. A
// lost packet of a stateless codec is concealed and re-encoded; of a
// stateful one (G.722), the far end has to conceal it, because a spliced
// bitstream would desynchronise its decoder.
func (d *direction) passOne(now time.Time) {
	for d.carry < d.outUnits {
		pkt, st, n := d.pop(now)
		d.carry += n
		switch {
		case st == jitter.OK:
			if !d.in.Stateful {
				d.plc.Good(d.dec.Decode(d.pcm[:0], pkt.Payload))
			}
			d.send(pkt.Payload, n)
			continue
		case d.in.Stateful:
			d.outLeg.Skip(n)
		case st == jitter.Lost:
			d.pcm = d.plc.Conceal(d.pcm[:0], d.samples(n))
			d.payload = d.enc.Encode(d.payload[:0], d.pcm)
			d.send(d.payload, n)
		default:
			d.pcm = append(d.pcm[:0], make([]int16, d.samples(n))...)
			d.plc.Good(d.pcm)
			d.payload = d.enc.Encode(d.payload[:0], d.pcm)
			d.send(d.payload, n)
		}
		if st == jitter.Lost {
			d.count(func(s *DirStats) { s.Concealed++ })
		} else {
			d.count(func(s *DirStats) { s.Silence++ })
		}
	}
	d.carry -= d.outUnits
}

func (d *direction) send(payload []byte, units uint32) {
	if err := d.outLeg.WritePacket(d.outPT, payload, units, d.marker); err != nil {
		d.count(func(s *DirStats) { s.SendErrs++ })
		return
	}
	d.marker = false
	d.count(func(s *DirStats) { s.Sent++ })
}

// count changes a counter under the lock Stats reads them with.
func (d *direction) count(fn func(*DirStats)) {
	d.mu.Lock()
	fn(&d.stats)
	d.mu.Unlock()
}

func (d *direction) snapshot(now time.Time) DirStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.stats
	s.Jitter = d.jb.Stats(now)
	return s
}

// Tick runs the clock once: every direction whose ptime has elapsed
// produces a frame. Run calls it every 10 ms; tests call it directly.
func (p *Pipeline) Tick(now time.Time) {
	for _, d := range []*direction{p.down, p.up} {
		d.due += tick
		for d.due >= d.ptime {
			d.due -= d.ptime
			d.produce(now)
		}
	}
}

// Receive feeds a packet that arrived on a leg at a given time: from the
// SIP leg it goes down, from the cell leg up. Run calls it from its
// readers; tests call it directly.
func (p *Pipeline) Receive(fromCell bool, pkt Packet, at time.Time) {
	if fromCell {
		p.up.push(pkt, at)
	} else {
		p.down.push(pkt, at)
	}
}

// Run reads both legs and runs the clock until ctx ends. The legs are not
// closed; their owner closes them after Run returns.
func (p *Pipeline) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.readerDone.Add(2)
	go p.read(ctx, p.cfg.SIP, false)
	go p.read(ctx, p.cfg.Cell, true)
	t := time.NewTicker(tick)
	defer t.Stop()
	p.log.Debug("media pipeline started", "plan", p.cfg.Plan.String())
	for {
		select {
		case <-ctx.Done():
			cancel()
			p.readerDone.Wait()
			return nil
		case now := <-t.C:
			p.Tick(now)
		}
	}
}

func (p *Pipeline) read(ctx context.Context, l Leg, fromCell bool) {
	defer p.readerDone.Done()
	for ctx.Err() == nil {
		pkt, err := l.ReadPacket(time.Now().Add(100 * time.Millisecond))
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				p.log.Debug("media leg read ended", "cell", fromCell, "err", err)
			}
			return
		}
		p.Receive(fromCell, pkt, time.Now())
	}
}

// SendDTMF sends a digit towards the SIP peer: as an RFC 4733 event when
// the peer negotiated telephone-event (audio pauses for its duration), and
// otherwise as tones in the uplink audio. SIP INFO is signalling, not
// media, and is sent by the SIP call (docs/sip-ua.md).
func (p *Pipeline) SendDTMF(ctx context.Context, digit rune, d time.Duration) error {
	if p.cfg.Events != nil {
		p.eventBusy.Store(true)
		defer p.eventBusy.Store(false)
		return p.cfg.Events.SendDTMF(ctx, digit, d)
	}
	p.up.mu.Lock()
	defer p.up.mu.Unlock()
	return p.up.dtmf.Add(digit, int(d/time.Millisecond))
}

// Stats are the pipeline's counters per direction.
type Stats struct {
	Plan     Plan
	Down, Up DirStats
}

// Stats returns the counters at now.
func (p *Pipeline) Stats(now time.Time) Stats {
	return Stats{Plan: p.cfg.Plan, Down: p.down.snapshot(now), Up: p.up.snapshot(now)}
}
