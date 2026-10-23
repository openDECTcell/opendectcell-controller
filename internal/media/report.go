// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"math"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/media/rtpstats"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
)

// Quality is a call's media quality as stored in call_log.quality
// (docs/media-engine.md §7). Both legs are reported separately, because a
// call can be fine on one and bad on the other, and the fix differs (the
// PBX or provider for the SIP leg, the LAN or the cell for the cell leg).
type Quality struct {
	SIP  LegQuality `json:"sip"`
	Cell LegQuality `json:"cell"`
	// Down is SIP → cell (what the handset user hears), Up the other way.
	Down DirQuality `json:"down"`
	Up   DirQuality `json:"up"`
	// MOS is an E-model estimate (ITU-T G.107, simplified) for the worse
	// direction, on the narrowband scale, 1.0–4.5.
	MOS float64 `json:"mos"`
	// WorseLeg names the leg that impairs the call more: "sip", "cell", or
	// "" when neither matters.
	WorseLeg string `json:"worse_leg,omitempty"`
	Plan     string `json:"plan"`
}

// LegQuality is one leg's network statistics.
type LegQuality struct {
	Packets  uint64  `json:"packets"`  // received
	LossPct  float64 `json:"loss_pct"` // of what we should have received
	JitterMs float64 `json:"jitter_ms"`
	// What the peer reported about our stream (RTCP; SIP leg only).
	PeerLossPct  *float64 `json:"peer_loss_pct,omitempty"`
	PeerJitterMs *float64 `json:"peer_jitter_ms,omitempty"`
	RTTMs        *float64 `json:"rtt_ms,omitempty"`
}

// DirQuality is one direction through the controller.
type DirQuality struct {
	Passthrough  bool    `json:"passthrough"`
	ConcealedPct float64 `json:"concealed_pct"` // frames concealed of frames sent
	BufferMs     float64 `json:"buffer_ms"`     // jitter-buffer target
	Late         uint64  `json:"late"`
}

// Fixed parts of the mouth-to-ear delay the controller cannot measure: DECT
// framing and the cell's own buffering (one 10 ms frame each way plus a
// 20 ms buffer), and packetisation (one 20 ms frame).
const (
	cellPathDelay = 40 * time.Millisecond
	packetDelay   = 20 * time.Millisecond
)

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func round(x float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(x*p) / p
}

func ptr(x float64) *float64 { return &x }

func legQuality(s rtpstats.Stats) LegQuality {
	return LegQuality{Packets: s.Received, LossPct: round(100*s.LossRate, 2), JitterMs: round(ms(s.Jitter), 1)}
}

func dirQuality(d DirStats) DirQuality {
	q := DirQuality{Passthrough: d.Passed, BufferMs: round(ms(d.Jitter.Target), 1), Late: d.Jitter.Late}
	if d.Sent > 0 {
		q.ConcealedPct = round(100*float64(d.Concealed)/float64(d.Sent), 2)
	}
	return q
}

// Summarise builds the quality record of a call from its SIP endpoint, its
// cell leg and its pipeline.
func Summarise(sip rtp.Stats, cell rtpstats.Stats, pipe Stats) Quality {
	q := Quality{SIP: legQuality(sip.Received), Cell: legQuality(cell), Down: dirQuality(pipe.Down), Up: dirQuality(pipe.Up), Plan: pipe.Plan.String()}
	var rtt time.Duration
	if !sip.Peer.At.IsZero() {
		q.SIP.PeerLossPct = ptr(round(100*sip.Peer.LossRate, 2))
		q.SIP.PeerJitterMs = ptr(round(ms(sip.Peer.Jitter), 1))
		if sip.Peer.RTT > 0 {
			rtt = sip.Peer.RTT
			q.SIP.RTTMs = ptr(round(ms(rtt), 1))
		}
	}
	// Loss as each listener experiences it: the handset user hears what the
	// SIP leg delivered to us (the cell link downstream has no reports in
	// v1); the far end hears what the cell delivered, then what the SIP
	// network lost on the way.
	sipPeer := 0.0
	if q.SIP.PeerLossPct != nil {
		sipPeer = *q.SIP.PeerLossPct
	}
	down := q.SIP.LossPct
	up := 100 * (1 - (1-q.Cell.LossPct/100)*(1-sipPeer/100))
	delay := rtt/2 + time.Duration(max(q.Down.BufferMs, q.Up.BufferMs)*float64(time.Millisecond)) + cellPathDelay + packetDelay
	q.MOS = round(min(EModelMOS(down, delay), EModelMOS(up, delay)), 2)
	q.WorseLeg = worse(q)
	return q
}

// worse picks the leg to blame: the one with more loss, if either loses
// 1 % or more; else the one with more jitter, if either has 20 ms or more.
func worse(q Quality) string {
	sipLoss := q.SIP.LossPct
	if q.SIP.PeerLossPct != nil {
		sipLoss = max(sipLoss, *q.SIP.PeerLossPct)
	}
	sipJitter := q.SIP.JitterMs
	if q.SIP.PeerJitterMs != nil {
		sipJitter = max(sipJitter, *q.SIP.PeerJitterMs)
	}
	switch {
	case max(sipLoss, q.Cell.LossPct) >= 1:
		if sipLoss >= q.Cell.LossPct {
			return "sip"
		}
		return "cell"
	case max(sipJitter, q.Cell.JitterMs) >= 20:
		if sipJitter >= q.Cell.JitterMs {
			return "sip"
		}
		return "cell"
	}
	return ""
}

// EModelMOS estimates the MOS from random packet loss (percent) and
// one-way mouth-to-ear delay, by the E-model (ITU-T G.107) with every
// other factor at its default: R = 93.2 − Id − Ie,eff. The codec is taken
// as G.711 with loss concealment (Ie = 0, Bpl = 25.1, ITU-T G.113
// Appendix I). It is an estimate on the narrowband scale; a G.722 call is
// rated as if it were narrowband.
func EModelMOS(lossPct float64, delay time.Duration) float64 {
	d := ms(delay)
	id := 0.024 * d
	if d > 177.3 {
		id += 0.11 * (d - 177.3)
	}
	const ie, bpl = 0.0, 25.1
	ieEff := ie + (95-ie)*lossPct/(lossPct+bpl)
	r := 93.2 - id - ieEff
	switch {
	case r <= 0:
		return 1
	case r >= 100:
		return 4.5
	}
	return 1 + 0.035*r + r*(r-60)*(100-r)*7e-6
}
