// SPDX-License-Identifier: AGPL-3.0-or-later

// Package quality measures audio: signal-to-noise ratio after alignment,
// loudness-envelope correlation, tone power, and level. The media engine's
// tests and the regression corpus use it to tell a working path from a
// silent or garbled one (docs/media-engine.md §8).
//
// The envelope correlation is the check the cell side found decisive
// (the maintainers' notes): a path that carries the wrong format still follows the
// source's loudness, a path that does not carry the source at all does not,
// and a tone cannot tell the two apart.
package quality

import (
	"math"
)

// RMS is the root-mean-square level of x.
func RMS(x []int16) float64 {
	if len(x) == 0 {
		return 0
	}
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(x)))
}

// DBFS is the RMS level of x relative to a full-scale sine (0 dBFS), or
// -inf for silence.
func DBFS(x []int16) float64 {
	return 20 * math.Log10(RMS(x)/(32767/math.Sqrt2))
}

// SNR is the ratio, in dB, of the reference's energy to the energy of the
// difference between got and the reference, with got delayed by lag samples
// relative to ref: got[i+lag] is compared with ref[i].
func SNR(ref, got []int16, lag int) float64 {
	var sig, noise float64
	for i := range ref {
		j := i + lag
		if j < 0 || j >= len(got) {
			continue
		}
		r, d := float64(ref[i]), float64(got[j])-float64(ref[i])
		sig += r * r
		noise += d * d
	}
	if noise == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(sig/noise)
}

// BestSNR searches lags 0..maxLag for the highest SNR and returns it with
// the lag, which is the path's delay in samples.
func BestSNR(ref, got []int16, maxLag int) (snr float64, lag int) {
	snr = math.Inf(-1)
	for l := 0; l <= maxLag; l++ {
		if s := SNR(ref, got, l); s > snr {
			snr, lag = s, l
		}
	}
	return snr, lag
}

// EnvelopeFloor is the lowest level an envelope records, in dBFS. Below it
// is silence for telephony purposes, and a codec's idle noise (A-law decodes
// silence to ±8, about -72 dBFS) must not count as a difference from a
// digitally silent source.
const EnvelopeFloor = -60.0

// Envelope is the loudness in dBFS of consecutive windows of win samples,
// floored at EnvelopeFloor.
func Envelope(x []int16, win int) []float64 {
	env := make([]float64, 0, len(x)/win)
	for i := 0; i+win <= len(x); i += win {
		env = append(env, max(DBFS(x[i:i+win]), EnvelopeFloor))
	}
	return env
}

// Correlation is Pearson's r between a and b over their common length; 0 if
// either is constant.
func Correlation(a, b []float64) float64 {
	n := min(len(a), len(b))
	if n == 0 {
		return 0
	}
	var ma, mb float64
	for i := 0; i < n; i++ {
		ma += a[i]
		mb += b[i]
	}
	ma /= float64(n)
	mb /= float64(n)
	var sab, saa, sbb float64
	for i := 0; i < n; i++ {
		da, db := a[i]-ma, b[i]-mb
		sab += da * db
		saa += da * da
		sbb += db * db
	}
	if saa == 0 || sbb == 0 {
		return 0
	}
	return sab / math.Sqrt(saa*sbb)
}

// EnvelopeCorrelation compares the loudness envelopes of ref and got (20 ms
// windows at rate) over lags up to maxLag windows, and returns the best r.
// Above about 0.8 the path carries the source; near zero or negative it
// does not, whatever the logs say.
func EnvelopeCorrelation(ref, got []int16, rate, maxLag int) float64 {
	win := rate / 50
	a, b := Envelope(ref, win), Envelope(got, win)
	best := math.Inf(-1)
	for l := 0; l <= maxLag && l < len(b); l++ {
		best = max(best, Correlation(a, b[l:]))
	}
	return best
}

// TonePower is the power of frequency f (Hz) in x sampled at rate, by the
// Goertzel algorithm, normalised so that a full-scale sine gives about 1.
func TonePower(x []int16, f float64, rate int) float64 {
	if len(x) == 0 {
		return 0
	}
	w := 2 * math.Pi * f / float64(rate)
	c := 2 * math.Cos(w)
	var s1, s2 float64
	for _, v := range x {
		s := float64(v)/32768 + c*s1 - s2
		s2, s1 = s1, s
	}
	p := s1*s1 + s2*s2 - c*s1*s2
	n := float64(len(x))
	return 4 * p / (n * n)
}

// DominantFrequency is the strongest of the candidate frequencies in x.
func DominantFrequency(x []int16, rate int, candidates []float64) float64 {
	var best, bp float64
	for _, f := range candidates {
		if p := TonePower(x, f, rate); p > bp {
			best, bp = f, p
		}
	}
	return best
}
