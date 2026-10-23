// SPDX-License-Identifier: AGPL-3.0-or-later

package quality

import (
	"math"
	"math/rand/v2"
)

// Deterministic test signals. Every generator takes its sample rate, so the
// same signal can be made at 8 and 16 kHz, and every random one takes a seed,
// so a corpus built from them is reproducible bit for bit.

// Sine is a tone of f Hz at level dBFS, n samples long.
func Sine(f float64, level float64, rate, n int) []int16 {
	a := 32767 * math.Pow(10, level/20)
	x := make([]int16, n)
	for i := range x {
		x[i] = int16(math.Round(a * math.Sin(2*math.Pi*f*float64(i)/float64(rate))))
	}
	return x
}

// Silence is n zero samples.
func Silence(n int) []int16 { return make([]int16, n) }

// Noise is white noise at level dBFS RMS.
func Noise(level float64, rate, n int, seed uint64) []int16 {
	r := rand.New(rand.NewPCG(seed, 1)) //nolint:gosec // reproducible test signal, not a secret
	a := 32767 / math.Sqrt2 * math.Pow(10, level/20)
	x := make([]int16, n)
	for i := range x {
		x[i] = clip(a * r.NormFloat64())
	}
	return x
}

// SpeechLike is a synthetic talker: a glottal pulse train with a gliding
// pitch, shaped by three vowel formants that change every syllable, under a
// syllabic loudness envelope with pauses. It is not speech, but it has
// speech's properties that matter to a codec, a jitter buffer and an
// envelope check: a harmonic spectrum that moves, energy concentrated below
// 4 kHz with some above, and a loudness range of tens of dB.
func SpeechLike(level float64, rate, n int, seed uint64) []int16 {
	r := rand.New(rand.NewPCG(seed, 2)) //nolint:gosec // reproducible test signal, not a secret
	vowels := [][3]float64{{730, 1090, 2440}, {270, 2290, 3010}, {300, 870, 2240}, {530, 1840, 2480}, {570, 840, 2410}}
	fs := float64(rate)
	// Two-pole resonators, one per formant, in cascade.
	type res struct{ a1, a2, g, y1, y2 float64 }
	set := func(rs *res, f, bw float64) {
		f = min(f, fs/2*0.9)
		rr := math.Exp(-math.Pi * bw / fs)
		rs.a1 = 2 * rr * math.Cos(2*math.Pi*f/fs)
		rs.a2 = -rr * rr
		rs.g = 1 - rr
	}
	var formants [3]res
	x := make([]float64, n)
	syl := 0       // samples left in this syllable
	sylLen := 0    // length of this syllable
	voiced := true // pause or syllable
	pitch, phase := 120.0, 0.0
	for i := range x {
		if syl == 0 {
			voiced = r.Float64() > 0.25
			sylLen = int(fs * (0.12 + 0.2*r.Float64()))
			syl = sylLen
			v := vowels[r.IntN(len(vowels))]
			for k := range formants {
				set(&formants[k], v[k], 80+40*float64(k))
			}
			pitch = 100 + 80*r.Float64()
		}
		syl--
		pos := float64(sylLen-syl) / float64(sylLen)
		env := 0.0
		if voiced {
			env = math.Sin(math.Pi * pos) // rises and falls within the syllable
		}
		phase += (pitch * (1 + 0.1*pos)) / fs
		var src float64
		if phase >= 1 {
			phase--
			src = 1
		}
		src += 0.02 * r.NormFloat64() // breath
		y := src
		for k := range formants {
			f := &formants[k]
			out := f.g*y + f.a1*f.y1 + f.a2*f.y2
			f.y2, f.y1 = f.y1, out
			y = out
		}
		x[i] = y * env
	}
	// Normalise to level dBFS over the voiced part.
	var s float64
	var m int
	for _, v := range x {
		if v != 0 {
			s += v * v
			m++
		}
	}
	g := 0.0
	if m > 0 {
		g = 32767 / math.Sqrt2 * math.Pow(10, level/20) / math.Sqrt(s/float64(m))
	}
	out := make([]int16, n)
	for i, v := range x {
		out[i] = clip(v * g)
	}
	return out
}

func clip(v float64) int16 {
	return int16(math.Round(max(-32768, min(32767, v))))
}
