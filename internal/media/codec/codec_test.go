// SPDX-License-Identifier: AGPL-3.0-or-later

package codec

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/media/quality"
)

func TestG711Exhaustive(t *testing.T) {
	for c := 0; c < 256; c++ {
		if got := AlawEncode(AlawDecode(byte(c))); got != byte(c) {
			t.Errorf("A-law %#02x decodes to %d, which encodes to %#02x", c, AlawDecode(byte(c)), got)
		}
		// µ-law has two zeros; 0x7F (negative zero) re-encodes as 0xFF.
		if got := UlawEncode(UlawDecode(byte(c))); got != byte(c) && c != 0x7F {
			t.Errorf("µ-law %#02x decodes to %d, which encodes to %#02x", c, UlawDecode(byte(c)), got)
		}
	}
}

// Golden values from ITU-T G.711 tables 1a/2a: the code for silence and the
// decision values at the ends of the range.
func TestG711Golden(t *testing.T) {
	for _, c := range []struct {
		code byte
		alaw int16
	}{{0xD5, 8}, {0x55, -8}, {0xAA, 32256}, {0x2A, -32256}} {
		if got := AlawDecode(c.code); got != c.alaw {
			t.Errorf("AlawDecode(%#02x) = %d, want %d", c.code, got, c.alaw)
		}
	}
	for _, c := range []struct {
		code byte
		ulaw int16
	}{{0xFF, 0}, {0x80, 32124}, {0x00, -32124}} {
		if got := UlawDecode(c.code); got != c.ulaw {
			t.Errorf("UlawDecode(%#02x) = %d, want %d", c.code, got, c.ulaw)
		}
	}
	if AlawEncode(0) != 0xD5 || UlawEncode(0) != 0xFF {
		t.Error("silence does not encode to 0xD5 / 0xFF")
	}
}

// Every 16-bit sample decodes to within half a quantisation step of itself,
// except where µ-law clips (above 32635).
func TestG711ErrorBound(t *testing.T) {
	for x := -32768; x <= 32767; x++ {
		s := int16(x)
		a := AlawDecode(AlawEncode(s))
		seg := int(AlawEncode(s)^0x55) >> 4 & 7
		if half := 8 << max(seg-1, 0); abs(int(a)-x) > half {
			t.Fatalf("A-law: %d → %d, error above half a step (%d)", x, a, half)
		}
		if abs(x) > ulawClip {
			continue
		}
		u := UlawDecode(UlawEncode(s))
		exp := int(^UlawEncode(s)>>4) & 7
		if half := 4 << exp; abs(int(u)-x) > half {
			t.Fatalf("µ-law: %d → %d, error above half a step (%d)", x, u, half)
		}
	}
}

func abs(x int) int { return max(x, -x) }

// G.711 keeps a speech-level signal at the SNR its 8-bit companding allows:
// about 37 dB for both at -20 dBFS.
func TestG711SNR(t *testing.T) {
	ref := quality.SpeechLike(-20, 8000, 8000, 1)
	for _, c := range []*Codec{PCMA, PCMU} {
		got := c.NewDecoder().Decode(nil, c.NewEncoder().Encode(nil, ref))
		if snr := quality.SNR(ref, got, 0); snr < 35 {
			t.Errorf("%s: SNR %.1f dB, want ≥ 35", c, snr)
		}
	}
}

func TestL16(t *testing.T) {
	pcm := []int16{0, 1, -1, 32767, -32768, 0x1234}
	b := L16.NewEncoder().Encode(nil, pcm)
	if hex.EncodeToString(b) != "00000001ffff7fff80001234" {
		t.Fatalf("L16 is not network byte order: %x", b)
	}
	got := L16.NewDecoder().Decode(nil, b)
	for i := range pcm {
		if got[i] != pcm[i] {
			t.Fatalf("L16 round trip: %v → %v", pcm, got)
		}
	}
}

// G.722 at 64 kbit/s is transparent to speech-band audio after its QMF delay,
// and keeps a tone's frequency: the check that would have caught a G.722
// stream misread as something else (a 400 Hz tone read as G.722 comes out
// near 3800 Hz).
func TestG722(t *testing.T) {
	ref := quality.SpeechLike(-20, 16000, 32000, 2)
	enc, dec := G722.NewEncoder(), G722.NewDecoder()
	var payload []byte
	var got []int16
	for i := 0; i < len(ref); i += 320 { // 20 ms frames, as the pipeline feeds it
		p := enc.Encode(nil, ref[i:i+320])
		if len(p) != 160 {
			t.Fatalf("20 ms of G.722 is %d octets, want 160", len(p))
		}
		payload = append(payload, p...)
		got = dec.Decode(got, p)
	}
	snr, lag := quality.BestSNR(ref, got, 64)
	if snr < 30 || lag != 22 {
		t.Errorf("G.722 SNR %.1f dB at lag %d, want ≥ 30 dB after the 22-sample QMF delay", snr, lag)
	}
	if r := quality.EnvelopeCorrelation(ref, got, 16000, 2); r < 0.95 {
		t.Errorf("G.722 envelope correlation %.3f, want ≥ 0.95", r)
	}
	tone := quality.Sine(400, -10, 16000, 16000)
	out := dec.Decode(nil, enc.Encode(nil, tone))
	if f := quality.DominantFrequency(out[1000:], 16000, []float64{400, 800, 3600, 3800, 7600}); f != 400 {
		t.Errorf("a 400 Hz tone comes out at %v Hz", f)
	}
	// Regression guard: the encoding of this deterministic input is fixed.
	sum := sha256.Sum256(payload)
	if got := hex.EncodeToString(sum[:8]); got != g722Golden {
		t.Errorf("G.722 encoding changed: %s, want %s", got, g722Golden)
	}
}

const g722Golden = "67cee9a6d76d23cf"

func TestCodecArithmetic(t *testing.T) {
	for _, c := range []struct {
		c        *Codec
		bytes    int
		tsPer20  uint32
		samples  int
		wideband bool
	}{{PCMA, 160, 160, 160, false}, {PCMU, 160, 160, 160, false}, {G722, 160, 160, 320, true}, {L16, 320, 160, 160, false}, {L16_16, 640, 320, 320, true}} {
		n := c.c.SampleRate / 50
		if n != c.samples || c.c.PayloadLen(n) != c.bytes || c.c.TimestampUnits(n) != c.tsPer20 || c.c.Samples(c.bytes) != n || c.c.Wideband() != c.wideband {
			t.Errorf("%s: 20 ms is %d samples, %d octets, %d timestamp units", c.c, n, c.c.PayloadLen(n), c.c.TimestampUnits(n))
		}
	}
	if _, err := ByName("opus"); err == nil {
		t.Error("ByName accepted an unsupported codec")
	}
	if c, err := ByName("G722"); err != nil || c != G722 {
		t.Error("ByName(G722)")
	}
}
