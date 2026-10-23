// SPDX-License-Identifier: AGPL-3.0-or-later

package codec

import "github.com/gotranspile/g722"

// G.722 at 64 kbit/s, 16 kHz audio, one octet per two samples. The codec is
// github.com/gotranspile/g722 (BSD and public domain), a Go translation of
// the CMU/Underwood implementation that Asterisk and FreeSWITCH also use, so
// it matches what the SIP peers decode (docs/media-engine.md §3).

type g722Enc struct{ e *g722.Encoder }
type g722Dec struct{ d *g722.Decoder }

func newG722Enc() Encoder { return g722Enc{g722.NewEncoder(g722.Rate64000, 0)} }
func newG722Dec() Decoder { return g722Dec{g722.NewDecoder(g722.Rate64000, 0)} }

func (e g722Enc) Encode(dst []byte, pcm []int16) []byte {
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
