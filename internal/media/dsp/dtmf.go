// SPDX-License-Identifier: AGPL-3.0-or-later

package dsp

import (
	"fmt"
	"math"
)

// DTMF frequencies (ITU-T Q.23): a low-group and a high-group tone per key.
var dtmfFreqs = map[rune][2]float64{
	'1': {697, 1209}, '2': {697, 1336}, '3': {697, 1477}, 'A': {697, 1633},
	'4': {770, 1209}, '5': {770, 1336}, '6': {770, 1477}, 'B': {770, 1633},
	'7': {852, 1209}, '8': {852, 1336}, '9': {852, 1477}, 'C': {852, 1633},
	'*': {941, 1209}, '0': {941, 1336}, '#': {941, 1477}, 'D': {941, 1633},
}

// DTMFFreqs returns the two frequencies of a key, for detectors in tests.
func DTMFFreqs(digit rune) ([2]float64, bool) {
	f, ok := dtmfFreqs[digit]
	return f, ok
}

// Tone levels: -10 dBm0 for the low group and -8 dBm0 for the high group
// (2 dB of twist, within Q.24). 0 dBm0 is a sine 3.14 dB below full scale
// (G.711 Table 5/6 digital milliwatt).
var (
	dtmfLow  = 32767 * math.Pow(10, (-10-3.14)/20)
	dtmfHigh = 32767 * math.Pow(10, (-8-3.14)/20)
)

// DTMFGap is the silence after each generated digit, so that repeated
// digits are detected as separate presses.
const DTMFGap = 60 // ms

type dtmfTone struct {
	f       [2]float64
	on, off int // samples
	elapsed int
}

// DTMFGen generates in-band DTMF digits into an audio stream. While a digit
// or its trailing gap plays, it replaces the stream's audio rather than
// mixing into it, so that speech cannot mask the tone at the far end's
// detector.
type DTMFGen struct {
	rate  int
	queue []dtmfTone
}

// NewDTMFGen makes a generator for the given sample rate.
func NewDTMFGen(rate int) *DTMFGen { return &DTMFGen{rate: rate} }

// Add queues a digit of the given length in ms (at least 40, the Q.24
// minimum for recognition; shorter requests are lengthened).
func (g *DTMFGen) Add(digit rune, ms int) error {
	f, ok := dtmfFreqs[digit]
	if !ok {
		return fmt.Errorf("dsp: %q is not a DTMF digit", digit)
	}
	g.queue = append(g.queue, dtmfTone{f: f, on: g.rate * max(ms, 40) / 1000, off: g.rate * DTMFGap / 1000})
	return nil
}

// Active reports whether a digit or its gap is playing or queued.
func (g *DTMFGen) Active() bool { return len(g.queue) > 0 }

// Fill overwrites frame with the tone, or the gap's silence, while a digit
// is active, and reports whether it did. A digit that ends inside the frame
// is followed by the next one, or silence, in the same frame.
func (g *DTMFGen) Fill(frame []int16) bool {
	if len(g.queue) == 0 {
		return false
	}
	for i := range frame {
		if len(g.queue) == 0 {
			frame[i] = 0
			continue
		}
		t := &g.queue[0]
		if t.elapsed < t.on {
			ph := float64(t.elapsed) / float64(g.rate) * 2 * math.Pi
			frame[i] = sat(dtmfLow*math.Sin(ph*t.f[0]) + dtmfHigh*math.Sin(ph*t.f[1]))
		} else {
			frame[i] = 0
		}
		t.elapsed++
		if t.elapsed >= t.on+t.off {
			g.queue = g.queue[1:]
		}
	}
	return true
}
