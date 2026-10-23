// SPDX-License-Identifier: AGPL-3.0-or-later

package codec

import "github.com/gotranspile/g722"

// G.722 at 64 kbit/s, 16 kHz audio, one octet per two samples. The codec is
// github.com/gotranspile/g722 (BSD and public domain), a Go translation of
// the CMU/Underwood implementation that Asterisk and FreeSWITCH also use, so
// it matches what the SIP peers decode (docs/media-engine.md §3).

// G722Headroom is the largest sample magnitude fed to the G.722 encoder.
// The encoder overloads within about 1 dB of full scale: a talker who clips
// at ±32767 comes out at 3.5 dB SNR, and at 26 dB once limited to ±30000
// (-0.8 dBFS). Samples below this are untouched, so normal speech is
// encoded bit for bit as before (docs/media-engine.md §3).
const G722Headroom = 30000

type g722Enc struct {
	e   *g722.Encoder
	buf *[]int16
}
type g722Dec struct{ d *g722.Decoder }

func newG722Enc() Encoder { return g722Enc{g722.NewEncoder(g722.Rate64000, 0), new([]int16)} }
func newG722Dec() Decoder { return g722Dec{g722.NewDecoder(g722.Rate64000, 0)} }

func (e g722Enc) Encode(dst []byte, pcm []int16) []byte {
	lim := (*e.buf)[:0]
	for _, s := range pcm {
		lim = append(lim, max(-G722Headroom, min(G722Headroom, s)))
	}
	*e.buf, pcm = lim, lim
	n := len(dst)
	dst = append(dst, make([]byte, (len(pcm)+1)/2)...)
	m := e.e.Encode(dst[n:], pcm)
	return dst[:n+m]
}

func (d g722Dec) Decode(dst []int16, p []byte) []int16 {
	n := len(dst)
	dst = append(dst, make([]int16, 2*len(p))...)
	m := d.d.Decode(dst[n:], p)
	return dst[:n+m]
}
