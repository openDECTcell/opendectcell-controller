// SPDX-License-Identifier: AGPL-3.0-or-later

package cdect

import (
	"bytes"
	"errors"
	"testing"
)

// feedAll pushes an Lc frame through the receiver one fragment at a time.
func feedAll(t *testing.T, r *LcRx, frame []byte) ([]byte, error) {
	t.Helper()
	if len(frame)%FragmentLen != 0 {
		t.Fatalf("frame length %d is not a multiple of %d", len(frame), FragmentLen)
	}
	for off := 0; off < len(frame); off += FragmentLen {
		got, err := r.Feed(frame[off : off+FragmentLen])
		if err != nil || got != nil {
			if off+FragmentLen != len(frame) {
				t.Fatalf("result after fragment at %d of %d", off, len(frame))
			}
			return got, err
		}
	}
	t.Fatal("no frame after the last fragment")
	return nil, nil
}

func TestRoundTripRR(t *testing.T) {
	r := NewLcRx(0)
	defer r.Close()

	rr, err := r.BuildRR(1, false)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := r.BuildFrame(rr, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := feedAll(t, r, frame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, rr) {
		t.Fatalf("round trip: got % x, want % x", got, rr)
	}
}

func TestLearnsLinkSignature(t *testing.T) {
	const lsig = 0x1101 // low 16 bits of an assigned TPUI, as seen on hardware
	r := NewLcRx(0)
	defer r.Close()

	rr, err := r.BuildRR(0, true)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := r.BuildFrame(rr, lsig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := feedAll(t, r, frame); err != nil {
		t.Fatal(err)
	}
	if r.LSIG() != lsig {
		t.Fatalf("LSIG = %#04x, want %#04x", r.LSIG(), lsig)
	}
}

func TestCorruptFrameDropped(t *testing.T) {
	r := NewLcRx(0)
	defer r.Close()

	rr, _ := r.BuildRR(1, false)
	frame, _ := r.BuildFrame(rr, 0)
	// Lock the signature with a good frame first, so the bad one cannot be
	// explained away as a different link signature.
	if _, err := feedAll(t, r, frame); err != nil {
		t.Fatal(err)
	}
	frame[len(frame)-1] ^= 0xff
	if _, err := feedAll(t, r, frame); !errors.Is(err, ErrDropped) {
		t.Fatalf("err = %v, want ErrDropped", err)
	}
}

func TestBoundsCheckedBeforeC(t *testing.T) {
	r := NewLcRx(0)
	defer r.Close()

	if _, err := r.Feed(make([]byte, FragmentLen+1)); !errors.Is(err, ErrLength) {
		t.Fatalf("oversized fragment: err = %v", err)
	}
	if _, err := r.BuildFrame(make([]byte, MaxLAPC+1), 0); !errors.Is(err, ErrLength) {
		t.Fatalf("oversized LAPC frame: err = %v", err)
	}
}

func TestUseAfterClose(t *testing.T) {
	r := NewLcRx(0)
	r.Close()
	r.Close()
	if _, err := r.Feed(make([]byte, FragmentLen)); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}
