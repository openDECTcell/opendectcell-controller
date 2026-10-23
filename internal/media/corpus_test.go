// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"math"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/media/codec"
	"github.com/openDECTcell/opendectcell-controller/internal/media/dsp"
	"github.com/openDECTcell/opendectcell-controller/internal/media/jitter"
	"github.com/openDECTcell/opendectcell-controller/internal/media/quality"
)

// The regression corpus (docs/media-engine.md §8). Every case pushes a known
// signal through the pipeline on virtual time and checks what comes out:
//
//   - absolutely: a signal comes out at its own level (±1 dB), so a dead
//     path or a level deficit fails, however healthy everything else looks;
//     silence stays silent; the loudness envelope follows the source; a tone
//     keeps its frequency;
//   - against testdata/corpus.golden.json: the metrics within a tolerance,
//     and on amd64 the exact output, by hash.
//
// The inputs are generated, so the corpus is reproducible bit for bit and
// no audio files are needed. Regenerate the golden file after an intended
// change with
//
//	go test ./internal/media -run Corpus -update-corpus
//
// and review the diff: every changed number is a changed behaviour.

var updateCorpus = flag.Bool("update-corpus", false, "rewrite testdata/corpus.golden.json")

const goldenPath = "testdata/corpus.golden.json"

// DefaultMaxDelayFrames is the jitter buffer's MaxDelay in 20 ms frames.
const DefaultMaxDelayFrames = int(jitter.DefaultMaxDelay / (20 * time.Millisecond))

type corpusSignal struct {
	name  string
	level float64 // dBFS of the voiced part; -inf for silence
	tone  float64 // Hz, for a tone
	gen   func(rate, n int) []int16
}

var corpusSignals = []corpusSignal{
	// A loud talker whose peaks clip at full scale: G.722 overloads on
	// that without the encoder's headroom limit (codec.G722Headroom).
	{"speech-10", -10, 0, func(r, n int) []int16 { return quality.SpeechLike(-10, r, n, 101) }},
	// The nominal active speech level (ITU-T P.56: -26 dBov) and a quiet
	// talker.
	{"speech-26", -26, 0, func(r, n int) []int16 { return quality.SpeechLike(-26, r, n, 102) }},
	{"speech-36", -36, 0, func(r, n int) []int16 { return quality.SpeechLike(-36, r, n, 103) }},
	{"tone1k-10", -10, 1000, func(r, n int) []int16 { return quality.Sine(1000, -10, r, n) }},
	{"noise-25", -25, 0, func(r, n int) []int16 { return quality.Noise(-25, r, n, 104) }},
	{"silence", math.Inf(-1), 0, func(_, n int) []int16 { return quality.Silence(n) }},
}

type corpusPath struct {
	name     string
	plan     Plan
	fromCell bool // uplink
	events   bool // SIP peer has telephone-event
}

func mustChoose(caps CellCaps, wb bool, sip *codec.Codec) Plan {
	p, err := Choose(caps, wb, sip)
	if err != nil {
		panic(err)
	}
	return p
}

var aLawUpCell = CellCaps{Down: []uint8{wire.FormatL16_8k}, Up: []uint8{wire.FormatPCMA}}

var corpusPaths = []corpusPath{
	{"pcma-l16-down", nbPlan, false, false},
	{"pcma-l16-up", nbPlan, true, false},
	{"pcmu-l16-down", mustChoose(narrowCell, false, codec.PCMU), false, false},
	{"g722-l16-down", mustChoose(narrowCell, true, codec.G722), false, false},
	{"g722-l16-up", mustChoose(narrowCell, true, codec.G722), true, false},
	{"g722-alaw-up", mustChoose(aLawUpCell, false, codec.G722), true, false},
	{"g722-pass-down", mustChoose(wideCell, true, codec.G722), false, true},
}

type corpusNet struct {
	name   string
	loss   float64
	burst  bool // losses come in runs of 3
	jitter time.Duration
}

var corpusNets = []corpusNet{
	{"clean", 0, false, 0},
	{"loss5", 0.05, false, 0},
	{"burst", 0.03, true, 0},
	{"jitter60", 0, false, 60 * time.Millisecond},
}

// corpusResult is what the golden file holds per case.
type corpusResult struct {
	LevelDB   float64 `json:"level_db"`   // of the output; -999 for silence
	EnvR      float64 `json:"env_r"`      // envelope correlation with the reference
	SNRdB     float64 `json:"snr_db"`     // at the best lag; clean only
	DelayMs   float64 `json:"delay_ms"`   // the lag; clean only
	Concealed uint64  `json:"concealed"`  // frames
	Hash      string  `json:"hash_amd64"` // of the output samples
}

// io returns the codecs at the pipeline's input and output for a path.
func (p corpusPath) io() (in, out *codec.Codec) {
	if p.fromCell {
		return FormatCodec(p.plan.CellUp), p.plan.SIP
	}
	return p.plan.SIP, FormatCodec(p.plan.CellDown)
}

func runCase(t *testing.T, sig corpusSignal, path corpusPath, nw corpusNet) (corpusResult, []int16, []int16, int) {
	in, out := path.io()
	const seconds = 3
	src := sig.gen(in.SampleRate, in.SampleRate*seconds)
	// The reference is the source at the output rate.
	ref := src
	switch {
	case in.SampleRate > out.SampleRate:
		ref = new(dsp.Down2).Process(nil, src)
	case in.SampleRate < out.SampleRate:
		ref = new(dsp.Up2).Process(nil, src)
	}
	var ev EventSender
	if path.events {
		ev = nopEvents{}
	}
	s := newSim(t, path.plan, ev)
	s.loss, s.jitter = nw.loss, nw.jitter
	if nw.burst {
		s.loss = 0
		s.lossRun = 3
		s.lossProb = nw.loss / 3
	}
	s.run(path.fromCell, in, src)
	leg := s.cell
	if path.fromCell {
		leg = s.sip
	}
	got := leg.decoded(out)
	st := s.p.Stats(s.start.Add(time.Hour))
	d := st.Down
	if path.fromCell {
		d = st.Up
	}
	r := corpusResult{LevelDB: -999, Concealed: d.Concealed}
	// Level over the whole signal, skipping the start-up delay.
	if l := quality.DBFS(got[min(len(got), out.SampleRate/5):]); !math.IsInf(l, -1) {
		r.LevelDB = round(l, 2)
	}
	r.EnvR = round(quality.EnvelopeCorrelation(ref, got, out.SampleRate, 15), 3)
	if nw.name == "clean" {
		snr, lag := quality.BestSNR(ref, got, out.SampleRate/5)
		switch {
		case math.IsInf(snr, 1):
			snr = 999
		case math.IsInf(snr, -1) || math.IsNaN(snr):
			snr = -999 // a silent reference
		}
		r.SNRdB = round(snr, 1)
		if sig.tone == 0 && !math.IsInf(sig.level, -1) {
			// A periodic signal has no unique delay.
			r.DelayMs = round(float64(lag)*1000/float64(out.SampleRate), 2)
		}
	}
	b := make([]byte, 2*len(got))
	for i, v := range got {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	h := sha256.Sum256(b)
	r.Hash = hex.EncodeToString(h[:8])
	return r, ref, got, out.SampleRate
}

func TestCorpus(t *testing.T) {
	if raceDetector {
		// It runs on one goroutine, so the detector finds nothing, and it is
		// ten times slower; CI runs it in a step of its own without -race.
		t.Skip("the corpus does not run under -race")
	}
	golden := map[string]corpusResult{}
	if b, err := os.ReadFile(goldenPath); err == nil {
		if err := json.Unmarshal(b, &golden); err != nil {
			t.Fatal(err)
		}
	} else if !*updateCorpus {
		t.Fatalf("no golden file: %v (run with -update-corpus)", err)
	}
	results := map[string]corpusResult{}
	for _, sig := range corpusSignals {
		for _, path := range corpusPaths {
			for _, nw := range corpusNets {
				name := sig.name + "/" + path.name + "/" + nw.name
				r, ref, got, rate := runCase(t, sig, path, nw)
				results[name] = r
				checkAbsolute(t, name, sig, nw, r, ref, got, rate)
				if *updateCorpus {
					continue
				}
				g, ok := golden[name]
				if !ok {
					t.Errorf("%s: not in the golden file (run with -update-corpus)", name)
					continue
				}
				checkGolden(t, name, g, r)
			}
		}
	}
	for name := range golden {
		if _, ok := results[name]; !ok && !*updateCorpus {
			t.Errorf("%s: in the golden file but no longer run", name)
		}
	}
	if *updateCorpus {
		b, err := json.MarshalIndent(results, "", "  ") // keys sorted
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, append(b, '\n'), 0o644); err != nil { //nolint:gosec // a test fixture
			t.Fatal(err)
		}
		t.Logf("wrote %d cases to %s", len(results), goldenPath)
	}
}

// checkAbsolute holds whatever the golden file says.
func checkAbsolute(t *testing.T, name string, sig corpusSignal, nw corpusNet, r corpusResult, ref, got []int16, rate int) {
	t.Helper()
	if math.IsInf(sig.level, -1) {
		if r.LevelDB > quality.EnvelopeFloor {
			t.Errorf("%s: silence came out at %.1f dBFS", name, r.LevelDB)
		}
		return
	}
	// The level, measured the same way on the reference: a dead path is
	// -999, a level deficit shows as a gap.
	want := quality.DBFS(ref[min(len(ref), rate/5):])
	tol := 1.0
	if nw.loss > 0 {
		tol = 1.5 // concealment fades long losses
	}
	if math.Abs(r.LevelDB-want) > tol {
		t.Errorf("%s: output level %.2f dBFS, input %.2f: the path loses or adds level", name, r.LevelDB, want)
	}
	minEnv := 0.95
	if nw.loss > 0 || nw.jitter > 0 {
		minEnv = 0.85
	}
	if sig.tone > 0 || sig.name == "noise-25" {
		minEnv = -1 // a steady signal has no envelope to follow
	}
	if r.EnvR < minEnv {
		t.Errorf("%s: envelope correlation %.3f, want ≥ %.2f", name, r.EnvR, minEnv)
	}
	if sig.tone > 0 {
		seg := got[min(len(got), rate/2):]
		if f := quality.DominantFrequency(seg, rate, []float64{sig.tone / 2, sig.tone, 2 * sig.tone, float64(rate)/2 - sig.tone}); f != sig.tone {
			t.Errorf("%s: a %v Hz tone came out at %v Hz", name, sig.tone, f)
		}
	}
	// On a clean network the only concealment is at the end: a stream that
	// stops looks like loss until it has been dry for MaxDelay (six frames).
	if tail := uint64(DefaultMaxDelayFrames); nw.name == "clean" && r.Concealed > tail {
		t.Errorf("%s: %d frames concealed on a clean network (the end accounts for %d)", name, r.Concealed, tail)
	}
}

func checkGolden(t *testing.T, name string, g, r corpusResult) {
	t.Helper()
	diff := func(what string, a, b, tol float64) {
		if math.Abs(a-b) > tol {
			t.Errorf("%s: %s %.3f, golden %.3f", name, what, b, a)
		}
	}
	diff("level", g.LevelDB, r.LevelDB, 0.1)
	diff("envelope r", g.EnvR, r.EnvR, 0.01)
	diff("SNR", g.SNRdB, r.SNRdB, 0.5)
	diff("delay ms", g.DelayMs, r.DelayMs, 0)
	diff("concealed", float64(g.Concealed), float64(r.Concealed), 0)
	if runtime.GOARCH == "amd64" && g.Hash != r.Hash {
		t.Errorf("%s: output changed (hash %s, golden %s)", name, r.Hash, g.Hash)
	}
}
