// SPDX-License-Identifier: AGPL-3.0-or-later

package dsp

import "math"

// Concealer hides lost frames by repeating the last pitch period of the
// signal, a simplified form of ITU-T G.711 Appendix I (waveform
// substitution):
//
//   - the pitch period is found by autocorrelation over the last 15 ms,
//     between 2.5 and 15 ms (66–400 Hz);
//   - the first 10 ms of a loss are played at full level, then the level
//     falls linearly to silence at 60 ms, so a long gap goes quiet rather
//     than buzzing;
//   - when real audio returns, the first 4 ms cross-fade from the synthetic
//     signal into it, so there is no click.
//
// It works at any sample rate. It is codec-independent and runs on the
// decoded signal; a G.722 decoder's internal state is not repaired, which
// G.722 Appendix III would do, and which costs a little quality for a few
// milliseconds after a loss.
type Concealer struct {
	rate   int
	hist   []int16 // the recent good (or concealed) signal, newest last
	period int     // pitch period of the current loss, 0 when not in one
	base   []int16 // the last period before the loss, repeated
	pos    int     // position in the repeated period
	lost   int     // samples concealed in the current loss
}

// NewConcealer makes a concealer for the given sample rate.
func NewConcealer(rate int) *Concealer {
	return &Concealer{rate: rate, hist: make([]int16, rate*48/1000)}
}

func (c *Concealer) ms(n int) int { return c.rate * n / 1000 }

// Good passes a received frame through, recording it as history. After a
// loss, the start of the frame is cross-faded from the synthetic signal; the
// frame is modified in place.
func (c *Concealer) Good(frame []int16) {
	if c.period > 0 {
		n := min(c.ms(4), len(frame))
		syn := c.synth(n, false)
		for i := range n {
			w := float64(i+1) / float64(n+1)
			frame[i] = sat((1-w)*float64(syn[i]) + w*float64(frame[i]))
		}
		c.period, c.lost = 0, 0
	}
	c.push(frame)
}

// Conceal appends n samples of synthetic signal to dst, standing in for a
// lost frame.
func (c *Concealer) Conceal(dst []int16, n int) []int16 {
	if c.period == 0 {
		c.period = c.pitch()
		c.pos = 0
	}
	out := c.synth(n, true)
	c.push(out)
	return append(dst, out...)
}

// Concealing reports whether the last frame was synthetic.
func (c *Concealer) Concealing() bool { return c.period > 0 }

// synth makes n samples continuing the repeated period. With advance, the
// loss position moves on and the fade applies; without, it is a look-ahead
// for the cross-fade.
func (c *Concealer) synth(n int, advance bool) []int16 {
	out := make([]int16, n)
	pos, lost := c.pos, c.lost
	full, end := c.ms(10), c.ms(60)
	for i := range out {
		g := 1.0
		if lost > full {
			g = max(0, 1-float64(lost-full)/float64(end-full))
		}
		out[i] = sat(g * float64(c.base[pos]))
		pos = (pos + 1) % c.period
		lost++
	}
	if advance {
		c.pos, c.lost = pos, lost
	}
	return out
}

func (c *Concealer) push(x []int16) {
	if len(x) >= len(c.hist) {
		copy(c.hist, x[len(x)-len(c.hist):])
		return
	}
	copy(c.hist, c.hist[len(x):])
	copy(c.hist[len(c.hist)-len(x):], x)
}

// pitch finds the period that best predicts the last 15 ms from the samples
// one period earlier, and freezes that period as the pattern to repeat.
func (c *Concealer) pitch() int {
	lo, hi := c.ms(5)/2, c.ms(15)
	win := c.ms(15)
	n := len(c.hist)
	best, bestScore := hi, -1.0
	for p := lo; p <= hi; p++ {
		var xy, yy float64
		for i := n - win; i < n; i++ {
			x, y := float64(c.hist[i]), float64(c.hist[i-p])
			xy += x * y
			yy += y * y
		}
		if yy == 0 {
			continue
		}
		if s := xy / math.Sqrt(yy); s > bestScore {
			best, bestScore = p, s
		}
	}
	c.base = append(c.base[:0], c.hist[n-best:]...)
	return best
}
