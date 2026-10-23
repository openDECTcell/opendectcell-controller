// SPDX-License-Identifier: AGPL-3.0-or-later

package dsp

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/media/quality"
)

// In-band audio survives 16 → 8 → 16 kHz: the telephone band passes, with
// the converters' delay.
func TestResampleRoundTrip(t *testing.T) {
	ref := quality.Sine(1000, -10, 16000, 16000)
	var d Down2
	var u Up2
	got := u.Process(nil, d.Process(nil, ref))
	if len(got) != len(ref) {
		t.Fatalf("16k → 8k → 16k: %d samples from %d", len(got), len(ref))
	}
	if snr := quality.SNR(ref[2000:14000], got[2000:], 2*Delay16k); snr < 40 {
		t.Errorf("round trip: SNR %.1f dB at the converters' delay, want ≥ 40 dB", snr)
	}
	// The delay itself, from an impulse (a sine cannot tell lags a period
	// apart).
	imp := make([]int16, 400)
	imp[100] = 16000
	var d2 Down2
	var u2 Up2
	resp := u2.Process(nil, d2.Process(nil, imp))
	peak := 0
	for i, v := range resp {
		if v > resp[peak] {
			peak = i
		}
	}
	if peak-100 != 2*Delay16k {
		t.Errorf("impulse delayed by %d samples, want %d", peak-100, 2*Delay16k)
	}
}

// Above 4 kHz, a 16 kHz signal must not fold back into the 8 kHz band.
func TestDown2Alias(t *testing.T) {
	for _, f := range []float64{4700, 6000, 7500} {
		var d Down2
		out := d.Process(nil, quality.Sine(f, 0, 16000, 16000))[200:]
		alias := 8000 - f // where it would land
		if p := quality.TonePower(out, alias, 8000); p > 1e-6 {
			t.Errorf("%v Hz aliases to %v Hz at %.1f dB", f, alias, 10*math.Log10(p))
		}
	}
	var d Down2
	out := d.Process(nil, quality.Sine(3400, -10, 16000, 16000))[200:]
	if l := quality.DBFS(out); math.Abs(l+10) > 0.5 {
		t.Errorf("3.4 kHz passes at %.2f dBFS, want -10", l)
	}
}

// Feeding a signal in frames gives the same result as feeding it at once:
// the converters keep their state, including an odd phase.
func TestResampleFraming(t *testing.T) {
	x := quality.SpeechLike(-20, 16000, 4800, 1)
	var d1, d2 Down2
	var u1, u2 Up2
	whole := u1.Process(nil, d1.Process(nil, x))
	var parts []int16
	for i := 0; i < len(x); {
		n := min(37+i%5, len(x)-i) // odd sizes on purpose
		parts = u2.Process(parts, d2.Process(nil, x[i:i+n]))
		i += n
	}
	if !slices.Equal(whole, parts) {
		t.Error("frame-by-frame resampling differs from one pass")
	}
}

// A voiced signal with a lost frame is concealed far better than by silence,
// and the join back to real audio is smooth.
func TestConcealVoiced(t *testing.T) {
	const rate, frame = 8000, 160
	ref := quality.Sine(200, -10, rate, rate) // a 40-sample period
	c := NewConcealer(rate)
	var out []int16
	for i := 0; i < len(ref); i += frame {
		f := slices.Clone(ref[i : i+frame])
		if i/frame == 20 {
			out = c.Conceal(out, frame)
			continue
		}
		c.Good(f)
		out = append(out, f...)
	}
	lost := ref[20*frame : 21*frame]
	if snr := quality.SNR(lost, out[20*frame:21*frame], 0); snr < 20 {
		t.Errorf("concealed frame: SNR %.1f dB, want ≥ 20", snr)
	}
	// No click at the joins: no sample-to-sample step bigger than the
	// signal's own steepest.
	maxStep := 0
	for i := 1; i < len(ref); i++ {
		maxStep = max(maxStep, abs(int(ref[i])-int(ref[i-1])))
	}
	for i := 20*frame - 2; i < 21*frame+40; i++ {
		if s := abs(int(out[i]) - int(out[i-1])); s > maxStep*3/2 {
			t.Errorf("step of %d at sample %d (signal's steepest is %d)", s, i, maxStep)
		}
	}
}

func abs(x int) int { return max(x, -x) }

// A long gap fades to silence by 60 ms instead of buzzing on.
func TestConcealLongLoss(t *testing.T) {
	c := NewConcealer(8000)
	c.Good(quality.Sine(150, -10, 8000, 480))
	var out []int16
	for range 5 {
		out = c.Conceal(out, 160)
	}
	if l := quality.DBFS(out[:80]); l < -11 {
		t.Errorf("first 10 ms of loss at %.1f dBFS, want full level", l)
	}
	if r := quality.RMS(out[480:]); r != 0 {
		t.Errorf("after 60 ms of loss the level is %.1f, want silence", r)
	}
	if !c.Concealing() {
		t.Error("Concealing() is false during a loss")
	}
	c.Good(make([]int16, 160))
	if c.Concealing() {
		t.Error("Concealing() is true after a good frame")
	}
}

// On a talker with random 5 % loss, concealment beats silence insertion and
// keeps the loudness envelope.
func TestConcealSpeech(t *testing.T) {
	const rate, frame = 8000, 160
	ref := quality.SpeechLike(-20, rate, rate*10, 3)
	r := rand.New(rand.NewPCG(4, 4)) //nolint:gosec // reproducible loss pattern
	c := NewConcealer(rate)
	var plc, zero []int16
	for i := 0; i+frame <= len(ref); i += frame {
		f := slices.Clone(ref[i : i+frame])
		if r.Float64() < 0.05 {
			plc = c.Conceal(plc, frame)
			zero = append(zero, make([]int16, frame)...)
			continue
		}
		zero = append(zero, f...)
		c.Good(f)
		plc = append(plc, f...)
	}
	sp, sz := quality.SNR(ref, plc, 0), quality.SNR(ref, zero, 0)
	if sp <= sz {
		t.Errorf("concealment (%.1f dB) is no better than silence (%.1f dB)", sp, sz)
	}
	if e := quality.EnvelopeCorrelation(ref, plc, rate, 0); e < 0.95 {
		t.Errorf("envelope correlation with 5 %% loss: %.3f", e)
	}
}

func TestDTMFGen(t *testing.T) {
	const rate = 8000
	g := NewDTMFGen(rate)
	if err := g.Add('x', 100); err == nil {
		t.Error("accepted a non-digit")
	}
	for _, d := range "1#" {
		if err := g.Add(d, 100); err != nil {
			t.Fatal(err)
		}
	}
	var out []int16
	for g.Active() {
		f := make([]int16, 160)
		if !g.Fill(f) {
			t.Fatal("Fill false while active")
		}
		out = append(out, f...)
	}
	// 100 ms on, 60 ms off, twice: 320 ms, rounded up to whole frames.
	if len(out) != 320*rate/1000 {
		t.Errorf("two digits took %d samples", len(out))
	}
	for i, d := range "1#" {
		seg := out[i*1280 : i*1280+800]
		f, _ := DTMFFreqs(d)
		low := quality.DominantFrequency(seg, rate, []float64{697, 770, 852, 941})
		high := quality.DominantFrequency(seg, rate, []float64{1209, 1336, 1477, 1633})
		if low != f[0] || high != f[1] {
			t.Errorf("digit %c detected as %v + %v Hz", d, low, high)
		}
		if gap := quality.RMS(out[i*1280+800 : (i+1)*1280]); gap != 0 {
			t.Errorf("gap after %c is not silent", d)
		}
	}
	if l := quality.DBFS(out[:800]); l > -6 || l < -12 {
		t.Errorf("tone level %.1f dBFS", l)
	}
	if g.Fill(make([]int16, 160)) {
		t.Error("Fill true when idle")
	}
}
