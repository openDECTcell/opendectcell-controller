// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"context"
	"errors"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/media/codec"
)

// What one end writes the other reads, numbered and timed by the writer; nothing is mixed up between the directions.
func TestRelayCarriesPackets(t *testing.T) {
	a, b := NewRelay()
	if err := a.WritePacket(9, []byte{1, 2}, 160, true); err != nil {
		t.Fatal(err)
	}
	a.Skip(320) // 40 ms of silence
	if err := a.WritePacket(9, []byte{3}, 160, false); err != nil {
		t.Fatal(err)
	}
	if err := b.WritePacket(8, []byte{9}, 80, false); err != nil {
		t.Fatal(err)
	}
	p1, err := b.ReadPacket(time.Now().Add(time.Second))
	if err != nil || p1.PT != 9 || p1.Seq != 0 || p1.TS != 0 || !p1.Marker || len(p1.Payload) != 2 {
		t.Fatalf("first %+v %v", p1, err)
	}
	p2, _ := b.ReadPacket(time.Now().Add(time.Second))
	if p2.Seq != 1 || p2.TS != 160+320 {
		t.Errorf("second %+v: the skip is not in the timestamp", p2)
	}
	if p1.SSRC == 0 || p1.SSRC != p2.SSRC {
		t.Errorf("ssrc %d %d", p1.SSRC, p2.SSRC)
	}
	back, err := a.ReadPacket(time.Now().Add(time.Second))
	if err != nil || back.PT != 8 || back.Payload[0] != 9 {
		t.Errorf("the other way: %+v %v", back, err)
	}
}

// A read with nothing to read times out in the way a socket does, and a closed end fails reads and writes.
func TestRelayTimeoutAndClose(t *testing.T) {
	a, b := NewRelay()
	if _, err := a.ReadPacket(time.Now().Add(20 * time.Millisecond)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("an empty read: %v", err)
	}
	CloseRelay(a)
	if _, err := a.ReadPacket(time.Now().Add(time.Second)); err == nil {
		t.Error("a closed end read")
	}
	if err := a.WritePacket(0, nil, 0, false); err == nil {
		t.Error("a closed end wrote")
	}
	// the open end writes into a closed peer without blocking, for as long as it likes
	for range 3 * relayDepth {
		if err := b.WritePacket(0, []byte{0}, 160, false); err != nil {
			t.Fatal(err)
		}
	}
}

// Two pipelines, one per cell, joined by a relay carry speech from one handset to the other: a
// narrowband handset's tone arrives at a wideband one (and the other way round) with its energy.
func TestRelayJoinsTwoPipelines(t *testing.T) {
	// cell A is narrowband, cell B wideband; they meet in G.711 (A-law), as the controller does for a mixed call
	a1, a2 := NewRelay() // a1: pipeline A's cell leg, a2: what the cell would hold
	b1, b2 := NewRelay()
	ra, rb := NewRelay()
	planA := Plan{SIP: codec.PCMA, CellDown: wire.FormatL16_8k, CellUp: wire.FormatL16_8k, Rate: 8000}
	planB := Plan{SIP: codec.PCMA, CellDown: wire.FormatL16_16k, CellUp: wire.FormatL16_16k, Rate: 8000}
	pa, err := New(Config{Plan: planA, SIP: ra, SIPPayloadType: 8, Cell: a1, CellPayload: 96})
	if err != nil {
		t.Fatal(err)
	}
	pb, err := New(Config{Plan: planB, SIP: rb, SIPPayloadType: 8, Cell: b1, CellPayload: 96})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, p := range []*Pipeline{pa, pb} {
		wg.Add(1)
		go func() { defer wg.Done(); _ = p.Run(ctx) }()
	}
	// A speaks a 440 Hz tone, 20 ms at a time, for one second
	pcm := func(n, rate int) []byte {
		b := make([]byte, 0, 2*n)
		for i := 0; i < n; i++ {
			v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
			b = append(b, byte(uint16(v)>>8), byte(v))
		}
		return b
	}
	// what B's cell gets (the pipeline sends a frame every 20 ms, silence when it has nothing): 16 kHz
	// linear, with the tone's energy in some of it
	var heard, silent int
	read := make(chan struct{})
	go func() {
		defer close(read)
		for ctx.Err() == nil {
			p, err := b2.ReadPacket(time.Now().Add(50 * time.Millisecond))
			if err != nil {
				continue
			}
			if len(p.Payload) != 640 {
				t.Errorf("a downlink frame of %d octets", len(p.Payload))
				return
			}
			var peak int
			for i := 0; i+1 < len(p.Payload); i += 2 {
				v := int(int16(uint16(p.Payload[i])<<8 | uint16(p.Payload[i+1])))
				if v < 0 {
					v = -v
				}
				peak = max(peak, v)
			}
			if peak > 2000 {
				heard++
			} else {
				silent++
			}
		}
	}()
	for i := 0; i < 50; i++ {
		_ = a2.WritePacket(96, pcm(160, 8000), 160, i == 0)
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // the last of it is still on its way
	cancel()
	<-read
	wg.Wait()
	if heard < 20 {
		t.Errorf("the tone reached the other cell in %d frames (%d silent)", heard, silent)
	}
}
