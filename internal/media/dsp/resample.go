// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dsp is the media engine's signal processing: rate conversion
// between 8 and 16 kHz, packet-loss concealment and DTMF tone generation
// (docs/media-engine.md §4–§6). Everything works on 16-bit linear PCM in
// frames and keeps its state between frames, so feeding a signal in pieces
// gives the same result as feeding it at once.
package dsp

import "math"

// The rate converter is a half-band low-pass FIR: a Kaiser-windowed sinc
// with its cutoff at a quarter of the 16 kHz rate (4 kHz). 47 taps with
// β = 5.65 give about 60 dB of stop-band rejection above 4.6 kHz, with the
// pass band flat to 3.4 kHz (the telephone band). Its delay is 23 samples at
// 16 kHz, 1.4 ms.
const (
	halfbandTaps = 47
	halfbandBeta = 5.65
)

// Delay16k is the converter's delay, in samples at 16 kHz, for either
// direction.
const Delay16k = (halfbandTaps - 1) / 2

var halfband = func() [halfbandTaps]float64 {
	var h [halfbandTaps]float64
	m := float64(halfbandTaps - 1)
	var sum float64
	for i := range h {
		n := float64(i) - m/2
		sinc := 0.5
		if n != 0 {
			sinc = math.Sin(math.Pi*n/2) / (math.Pi * n)
		}
		r := 2*float64(i)/m - 1
		h[i] = sinc * bessel0(halfbandBeta*math.Sqrt(1-r*r)) / bessel0(halfbandBeta)
		sum += h[i]
	}
	for i := range h {
		h[i] /= sum // unity gain at DC
	}
	return h
}()

// bessel0 is the zeroth-order modified Bessel function of the first kind.
func bessel0(x float64) float64 {
	sum, term := 1.0, 1.0
	for k := 1; k < 50; k++ {
		term *= (x / 2) / float64(k)
		sum += term * term
		if term*term < 1e-12*sum {
			break
		}
	}
	return sum
}

func sat(v float64) int16 {
	return int16(math.Round(max(-32768, min(32767, v))))
}

// Down2 converts 16 kHz to 8 kHz.
type Down2 struct {
	hist [halfbandTaps]float64 // the last input samples, newest last
	odd  bool                  // the next input sample produces no output
}

// Process appends the 8 kHz version of in to dst. An odd-length input is
// fine; the phase carries over to the next call.
func (d *Down2) Process(dst, in []int16) []int16 {
	for _, s := range in {
		copy(d.hist[:], d.hist[1:])
		d.hist[halfbandTaps-1] = float64(s)
		d.odd = !d.odd
		if !d.odd {
			continue
		}
		var acc float64
		for k, h := range halfband {
			acc += h * d.hist[halfbandTaps-1-k]
		}
		dst = append(dst, sat(acc))
	}
	return dst
}

// Up2 converts 8 kHz to 16 kHz.
type Up2 struct {
	hist [(halfbandTaps + 1) / 2]float64 // the previous input samples, newest last
}

// Process appends the 16 kHz version of in to dst: two output samples per
// input sample.
func (u *Up2) Process(dst, in []int16) []int16 {
	for _, s := range in {
		copy(u.hist[:], u.hist[1:])
		u.hist[len(u.hist)-1] = float64(s)
		// Zero-stuffed input filtered with gain 2, split into its two
		// phases: even taps for one output, odd taps for the other.
		var a, b float64
		for k := 0; k < halfbandTaps; k++ {
			x := u.hist[len(u.hist)-1-k/2]
			if k%2 == 0 {
				a += halfband[k] * x
			} else {
				b += halfband[k] * x
			}
		}
		dst = append(dst, sat(2*a), sat(2*b))
	}
	return dst
}
