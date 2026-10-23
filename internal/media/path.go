// SPDX-License-Identifier: AGPL-3.0-or-later

// Package media is the media engine (C5, docs/media-engine.md): it bridges
// one call's audio between the cell leg (RTP to the cell, in the formats the
// cell reported) and the SIP leg (RTP or SRTP to the SIP peer, in the
// negotiated codec), with a jitter buffer, loss concealment, transcoding,
// DTMF and statistics in each direction.
package media

import (
	"errors"
	"fmt"
	"slices"

	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/media/codec"
)

// CellCaps is what a cell announced about audio in HELLO
// (cell-protocol.md §6.1, §8.2).
type CellCaps struct {
	Down []uint8 // formats the cell accepts, preferred first (tag 12)
	Up   []uint8 // formats the cell sends, preferred first (tag 16, or tag 12)
	// WidebandEndpoints is cap_audio_wb: host audio endpoints for wideband.
	WidebandEndpoints uint8
}

// FormatCodec is the codec of a cell-link format, nil for a format the
// media engine cannot carry (PCMA at 16 kHz, G.726, experimental).
func FormatCodec(f uint8) *codec.Codec {
	switch f {
	case wire.FormatL16_8k:
		return codec.L16
	case wire.FormatL16_16k:
		return codec.L16_16
	case wire.FormatPCMA:
		return codec.PCMA
	case wire.FormatPCMU:
		return codec.PCMU
	case wire.FormatG722:
		return codec.G722
	}
	return nil
}

func usable(list []uint8, wideband bool) []uint8 {
	var out []uint8
	for _, f := range list {
		if c := FormatCodec(f); c != nil && c.Wideband() == wideband {
			out = append(out, f)
		}
	}
	return out
}

// Wideband reports whether the cell can carry a wideband call: a wideband
// format in both directions and a wideband audio endpoint. On current cells
// it is false (openDECTcell notes, 2026-10-23). The DECT core must offer G.722 in
// <<CODEC-LIST>> only to a handset on a cell for which this is true.
func (c CellCaps) Wideband() bool {
	return c.WidebandEndpoints > 0 && len(usable(c.Down, true)) > 0 && len(usable(c.Up, true)) > 0
}

// Plan is how one call's audio flows.
type Plan struct {
	SIP      *codec.Codec // negotiated on the SIP leg
	CellDown uint8        // cell-link format, controller → cell
	CellUp   uint8        // cell-link format, cell → controller
	// Rate is the internal sample rate: 16 kHz only when both legs are
	// wideband, so nothing is resampled up only to be resampled down.
	Rate int
	// Passthrough per direction: the payload is forwarded as received.
	PassDown, PassUp bool
}

// Wideband reports whether the cell leg is wideband.
func (p Plan) Wideband() bool { return FormatCodec(p.CellDown).Wideband() }

func (p Plan) String() string {
	mode := func(pass bool) string {
		if pass {
			return "pass"
		}
		return "transcode"
	}
	return fmt.Sprintf("SIP %s, cell down %s (%s), up %s (%s), %d Hz", p.SIP, FormatCodec(p.CellDown), mode(p.PassDown),
		FormatCodec(p.CellUp), mode(p.PassUp), p.Rate)
}

// ErrNoFormat means the cell lists no format the media engine can carry in
// one of the directions; MEDIA_OPEN would fail with "format unsupported".
var ErrNoFormat = errors.New("media: the cell reports no usable audio format")

// Choose plans a call. wideband says whether the handset and the FP agreed
// on a wideband air codec; it only has effect when the cell can carry
// wideband, and otherwise the call is narrowband. In each direction the
// format equal to the SIP codec is taken if the cell lists it (no
// transcoding), else the cell's first usable entry.
func Choose(caps CellCaps, wideband bool, sip *codec.Codec) (Plan, error) {
	wb := wideband && caps.Wideband()
	pick := func(list []uint8) (uint8, error) {
		u := usable(list, wb)
		if len(u) == 0 {
			return 0, ErrNoFormat
		}
		if i := slices.IndexFunc(u, func(f uint8) bool { return FormatCodec(f) == sip }); i >= 0 {
			return u[i], nil
		}
		return u[0], nil
	}
	var p Plan
	var err error
	if p.CellDown, err = pick(caps.Down); err != nil {
		return p, fmt.Errorf("%w (downlink)", err)
	}
	if p.CellUp, err = pick(caps.Up); err != nil {
		return p, fmt.Errorf("%w (uplink)", err)
	}
	p.SIP = sip
	p.Rate = 8000
	if wb && sip.Wideband() {
		p.Rate = 16000
	}
	p.PassDown = FormatCodec(p.CellDown) == sip
	p.PassUp = FormatCodec(p.CellUp) == sip
	return p, nil
}
