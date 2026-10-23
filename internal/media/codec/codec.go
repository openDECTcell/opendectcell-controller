// SPDX-License-Identifier: AGPL-3.0-or-later

// Package codec converts between signed 16-bit linear PCM, the media
// engine's internal representation, and the payload formats on the SIP and
// cell legs: G.711 A-law and µ-law, G.722 and L16 (docs/media-engine.md §3).
//
// Encoders and decoders carry state across calls (G.722 is adaptive), so
// each direction of each call needs its own instance, fed in order.
package codec

import "fmt"

// Encoder turns linear PCM at the codec's sample rate into payload octets.
type Encoder interface {
	// Encode appends the encoding of pcm to dst and returns it.
	Encode(dst []byte, pcm []int16) []byte
}

// Decoder turns payload octets into linear PCM at the codec's sample rate.
type Decoder interface {
	// Decode appends the decoding of payload to dst and returns it.
	Decode(dst []int16, payload []byte) []int16
}

// Codec describes one payload format.
type Codec struct {
	Name       string // as in SDP rtpmap and the account's codec list
	SampleRate int    // of the audio
	ClockRate  int    // of the RTP timestamp; 8000 for G.722 (RFC 3551 §4.5.2)
	// BitsPerSample is the payload size per audio sample.
	BitsPerSample int
	newEnc        func() Encoder
	newDec        func() Decoder
}

// NewEncoder returns a fresh encoder.
func (c *Codec) NewEncoder() Encoder { return c.newEnc() }

// NewDecoder returns a fresh decoder.
func (c *Codec) NewDecoder() Decoder { return c.newDec() }

// Samples is the number of audio samples in a payload of n octets.
func (c *Codec) Samples(n int) int { return n * 8 / c.BitsPerSample }

// PayloadLen is the payload size of a frame of samples audio samples.
func (c *Codec) PayloadLen(samples int) int { return samples * c.BitsPerSample / 8 }

// TimestampUnits converts audio samples to RTP timestamp units.
func (c *Codec) TimestampUnits(samples int) uint32 {
	return uint32(samples * c.ClockRate / c.SampleRate)
}

// Wideband reports whether the codec carries 16 kHz audio.
func (c *Codec) Wideband() bool { return c.SampleRate == 16000 }

func (c *Codec) String() string { return c.Name }

// The supported codecs.
var (
	PCMA   = &Codec{Name: "PCMA", SampleRate: 8000, ClockRate: 8000, BitsPerSample: 8, newEnc: func() Encoder { return alawEnc{} }, newDec: func() Decoder { return alawDec{} }}
	PCMU   = &Codec{Name: "PCMU", SampleRate: 8000, ClockRate: 8000, BitsPerSample: 8, newEnc: func() Encoder { return ulawEnc{} }, newDec: func() Decoder { return ulawDec{} }}
	G722   = &Codec{Name: "G722", SampleRate: 16000, ClockRate: 8000, BitsPerSample: 4, newEnc: newG722Enc, newDec: newG722Dec}
	L16    = &Codec{Name: "L16", SampleRate: 8000, ClockRate: 8000, BitsPerSample: 16, newEnc: func() Encoder { return l16{} }, newDec: func() Decoder { return l16{} }}
	L16_16 = &Codec{Name: "L16/16000", SampleRate: 16000, ClockRate: 16000, BitsPerSample: 16, newEnc: func() Encoder { return l16{} }, newDec: func() Decoder { return l16{} }}
)

// ByName finds a codec by its SDP name (G722, PCMA, PCMU).
func ByName(name string) (*Codec, error) {
	for _, c := range []*Codec{PCMA, PCMU, G722, L16, L16_16} {
		if c.Name == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("codec: unknown codec %q", name)
}

// l16 is RFC 3551 L16: signed 16-bit samples, network byte order.
type l16 struct{}

func (l16) Encode(dst []byte, pcm []int16) []byte {
	for _, s := range pcm {
		dst = append(dst, byte(uint16(s)>>8), byte(s))
	}
	return dst
}

func (l16) Decode(dst []int16, p []byte) []int16 {
	for i := 0; i+1 < len(p); i += 2 {
		dst = append(dst, int16(uint16(p[i])<<8|uint16(p[i+1])))
	}
	return dst
}
