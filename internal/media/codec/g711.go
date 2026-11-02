// SPDX-License-Identifier: AGPL-3.0-or-later

package codec

import "math/bits"

// G.711 (ITU-T G.711, 11/88) from its segment definition: eight segments of
// sixteen steps each, the step doubling per segment. A-law codes have the
// even bits inverted (XOR 0x55); µ-law codes are all inverted.

// AlawEncode encodes one sample.
func AlawEncode(s int16) byte {
	v := int(s) >> 3 // A-law works on 13 bits
	mask := byte(0xD5)
	if v < 0 {
		mask = 0x55
		v = -v - 1
	}
	// Segment: 0 for v < 32, then one per doubling up to 4095.
	seg := bits.Len(uint(v>>4)) - 1
	if seg < 0 {
		seg = 0
	}
	var a byte
	if seg == 0 {
		a = byte(v>>1) & 0x0F
	} else {
		a = byte(seg<<4) | byte(v>>seg)&0x0F
	}
	return a ^ mask
}

// AlawDecode decodes one code to the middle of its step.
func AlawDecode(a byte) int16 {
	a ^= 0x55
	t := int(a&0x0F)<<4 + 8
	if seg := int(a&0x70) >> 4; seg > 0 {
		t = (t + 0x100) << (seg - 1)
	}
	if a&0x80 == 0 {
		t = -t
	}
	return int16(t)
}

const (
	ulawBias = 0x84
	ulawClip = 32635
)

// UlawEncode encodes one sample.
func UlawEncode(s int16) byte {
	v := int(s)
	var sign byte
	if v < 0 {
		v, sign = -v, 0x80
	}
	v = min(v, ulawClip) + ulawBias
	exp := bits.Len(uint(v>>7)) - 1 // v>>7 is 1..255
	mant := byte(v>>(exp+3)) & 0x0F
	return ^(sign | byte(exp<<4) | mant)
}

// UlawDecode decodes one code to the middle of its step.
func UlawDecode(u byte) int16 {
	u = ^u
	exp := (u >> 4) & 7
	v := (int(u&0x0F)<<3+ulawBias)<<exp - ulawBias
	if u&0x80 != 0 {
		v = -v
	}
	return int16(v)
}

type alawEnc struct{}
type alawDec struct{}
type ulawEnc struct{}
type ulawDec struct{}

// Encode appends one A-law octet per sample (G.711).
func (alawEnc) Encode(dst []byte, pcm []int16) []byte {
	for _, s := range pcm {
		dst = append(dst, AlawEncode(s))
	}
	return dst
}

// Decode appends one 16-bit sample per A-law octet.
func (alawDec) Decode(dst []int16, p []byte) []int16 {
	for _, c := range p {
		dst = append(dst, AlawDecode(c))
	}
	return dst
}

// Encode appends one mu-law octet per sample (G.711).
func (ulawEnc) Encode(dst []byte, pcm []int16) []byte {
	for _, s := range pcm {
		dst = append(dst, UlawEncode(s))
	}
	return dst
}

// Decode appends one 16-bit sample per mu-law octet.
func (ulawDec) Decode(dst []int16, p []byte) []int16 {
	for _, c := range p {
		dst = append(dst, UlawDecode(c))
	}
	return dst
}
