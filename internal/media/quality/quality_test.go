// SPDX-License-Identifier: AGPL-3.0-or-later

package quality

import (
	"math"
	"slices"
	"testing"
)

func TestLevels(t *testing.T) {
	if l := DBFS(Sine(1000, -20, 8000, 8000)); math.Abs(l+20) > 0.1 {
		t.Errorf("a -20 dBFS sine measures %.2f dBFS", l)
	}
	if l := DBFS(Noise(-30, 8000, 8000, 1)); math.Abs(l+30) > 0.5 {
		t.Errorf("-30 dBFS noise measures %.2f dBFS", l)
	}
	if !math.IsInf(DBFS(Silence(160)), -1) {
		t.Error("silence has a level")
	}
}

func TestSNR(t *testing.T) {
	x := SpeechLike(-20, 8000, 4000, 1)
	if !math.IsInf(SNR(x, x, 0), 1) {
		t.Error("identical signals have finite SNR")
	}
	d := append(Silence(7), x...)
	if s, l := BestSNR(x, d, 20); l != 7 || !math.IsInf(s, 1) {
		t.Errorf("BestSNR found lag %d (%.1f dB), want 7", l, s)
	}
}

// The property the cell side relied on: the envelope follows a delayed or
// distorted copy of the source, and does not follow unrelated audio of a
// similar character.
func TestEnvelopeCorrelation(t *testing.T) {
	src := SpeechLike(-20, 8000, 80000, 1)
	delayed := append(Silence(400), src...)
	if r := EnvelopeCorrelation(src, delayed, 8000, 5); r < 0.95 { // 2.5 windows: not window-aligned
		t.Errorf("delayed copy: r = %.3f", r)
	}
	quiet := slices.Clone(src)
	for i := range quiet {
		quiet[i] /= 16 // 24 dB down: a level deficit is still our data
	}
	if r := EnvelopeCorrelation(src, quiet, 8000, 0); r < 0.95 {
		t.Errorf("attenuated copy: r = %.3f", r)
	}
	other := SpeechLike(-20, 8000, 80000, 99)
	if r := EnvelopeCorrelation(src, other, 8000, 0); r > 0.5 {
		t.Errorf("unrelated talker: r = %.3f", r)
	}
	if r := EnvelopeCorrelation(src, Noise(-30, 8000, 80000, 2), 8000, 0); r > 0.3 {
		t.Errorf("steady noise: r = %.3f", r)
	}
}

func TestTones(t *testing.T) {
	x := Sine(697, -10, 8000, 800)
	if p := TonePower(x, 697, 8000); math.Abs(p-0.1) > 0.02 {
		t.Errorf("TonePower = %.3f, want about 0.1 (-10 dBFS)", p)
	}
	if f := DominantFrequency(x, 8000, []float64{697, 770, 852, 941}); f != 697 {
		t.Errorf("DominantFrequency = %v", f)
	}
}

func TestSpeechLikeIsDeterministic(t *testing.T) {
	if !slices.Equal(SpeechLike(-20, 16000, 1000, 5), SpeechLike(-20, 16000, 1000, 5)) {
		t.Error("same seed, different signal")
	}
	x := SpeechLike(-20, 8000, 80000, 5)
	// Speech has a wide loudness range; a test talker without one would let
	// a broken path pass the envelope check.
	env := Envelope(x, 160)
	slices.Sort(env)
	p5, p95 := env[len(env)*5/100], env[len(env)*95/100]
	if p95-p5 < 30 {
		t.Errorf("loudness range %.1f dB, want ≥ 30", p95-p5)
	}
}
