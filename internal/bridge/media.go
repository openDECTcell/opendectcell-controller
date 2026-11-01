// SPDX-License-Identifier: AGPL-3.0-or-later

package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/core"
	"github.com/openDECTcell/opendectcell-controller/internal/media"
	"github.com/openDECTcell/opendectcell-controller/internal/media/codec"
)

// The speech of a call (docs/media-engine.md §9, cell-protocol.md §8): once
// the chip's audio channel is bound (core.CallMediaUp) and the SIP side is
// answered, the controller listens on a UDP port, tells the cell with
// MEDIA_OPEN to tie the channel to it (SRTP, keys made here), and runs a
// media pipeline between that cell leg and the SIP call's RTP endpoint.

// mediaSession is one call's speech.
type mediaSession struct {
	id     uint32
	cell   uuid.UUID
	plan   media.Plan
	leg    *media.CellLeg
	pipe   *media.Pipeline
	cancel context.CancelFunc
	done   chan struct{}
}

type calllogEnd struct {
	cellCodec string
	quality   *media.Quality
}

// summary is the call's media quality and cell codec for the call log.
func (s *mediaSession) summary(c SIPCall) *calllogEnd {
	q := media.Summarise(c.Media().Stats(), s.leg.Stats(), s.pipe.Stats(time.Now()))
	return &calllogEnd{cellCodec: codecOfFormat(s.plan.CellDown), quality: &q}
}

func codecOfFormat(f uint8) string {
	if c := media.FormatCodec(f); c != nil {
		return c.Name
	}
	return fmt.Sprintf("format %d", f)
}

// cellPayloadType is the RTP payload type on the cell link: the static one of
// a standard format, a dynamic one for L16 (cell-protocol.md §8.2).
func cellPayloadType(plan media.Plan) uint8 {
	if plan.CellDown != plan.CellUp {
		return 96
	}
	switch plan.CellDown {
	case wire.FormatPCMA:
		return 8
	case wire.FormatPCMU:
		return 0
	case wire.FormatG722:
		return 9
	}
	return 96
}

var (
	errNoMediaIP  = errors.New("bridge: no address for the cells' speech is configured (cells.media_ip)")
	errNoNegotiat = errors.New("bridge: the SIP call has no negotiated media")
)

// openMedia opens the speech of a call whose audio channel is bound.
func (m *Manager) openMedia(ctx context.Context, ev core.CallEvent, c SIPCall) (*mediaSession, error) {
	neg := c.Negotiated()
	if neg == nil {
		return nil, errNoNegotiat
	}
	if !m.MediaIP.IsValid() {
		return nil, errNoMediaIP
	}
	sipCodec, err := codec.ByName(neg.Codec.Name)
	if err != nil {
		return nil, err
	}
	st := m.Cells.CellStatus(ev.Cell)
	caps := media.CellCaps{Down: st.AudioDown, Up: st.AudioUp, WidebandEndpoints: st.CapAudioWB}
	plan, err := media.Choose(caps, ev.Wideband, sipCodec)
	if err != nil {
		return nil, err
	}
	conn, port, err := m.listenMedia()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	if m.pending == nil {
		m.pending, m.sessions = map[uint32]chan wire.MediaOpenAckMsg{}, map[uint32]*mediaSession{}
	}
	ack := make(chan wire.MediaOpenAckMsg, 1)
	m.pending[id] = ack
	m.mu.Unlock()
	drop := func() {
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
	}
	mo, err := media.NewMediaOpen(id, ev.Channel, plan, cellPayloadType(plan), netip.AddrPortFrom(m.MediaIP, port), wire.SRTPAES128SHA80)
	if err != nil {
		drop()
		conn.Close()
		return nil, err
	}
	leg, err := media.NewCellLeg(conn, mo, media.FormatCodec(plan.CellUp).ClockRate)
	if err != nil {
		drop()
		conn.Close()
		return nil, err
	}
	if err := m.Cells.OpenMedia(ev.Cell, mo); err != nil {
		drop()
		leg.Close()
		return nil, err
	}
	wait := m.OpenTimeout
	if wait == 0 {
		wait = defaultOpenTimeout
	}
	select {
	case a := <-ack:
		drop()
		if a.Status != wire.MediaOK {
			leg.Close()
			return nil, fmt.Errorf("the cell refused the media (status %d)", a.Status)
		}
	case <-time.After(wait):
		drop()
		_ = m.Cells.CloseMedia(ev.Cell, id)
		leg.Close()
		return nil, errors.New("the cell did not answer MEDIA_OPEN")
	case <-ctx.Done():
		drop()
		_ = m.Cells.CloseMedia(ev.Cell, id)
		leg.Close()
		return nil, ctx.Err()
	}
	cfg := media.Config{Plan: plan, SIP: media.SIPLeg(c.Media()), SIPPayloadType: neg.Codec.PT,
		SIPPtime: time.Duration(neg.PTime) * time.Millisecond, Cell: leg, CellPayload: cellPayloadType(plan), Log: m.log()}
	if neg.TelEventPT != 0 {
		cfg.Events = c.Media()
	}
	pipe, err := media.New(cfg)
	if err != nil {
		_ = m.Cells.CloseMedia(ev.Cell, id)
		leg.Close()
		return nil, err
	}
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &mediaSession{id: id, cell: ev.Cell, plan: plan, leg: leg, pipe: pipe, cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	go func() {
		defer close(s.done)
		if err := pipe.Run(rctx); err != nil && !errors.Is(err, context.Canceled) {
			m.log().Info("a call's media pipeline ended", "err", err)
		}
	}()
	m.log().Info("a call's speech is open", "cell", ev.Cell, "media", id, "plan", plan.String(), "channel", ev.Channel)
	return s, nil
}

// closeMedia ends a call's speech: the pipeline, then the cell's flow.
func (m *Manager) closeMedia(s *mediaSession) {
	m.mu.Lock()
	_, live := m.sessions[s.id]
	delete(m.sessions, s.id)
	m.mu.Unlock()
	if !live {
		return
	}
	s.cancel()
	<-s.done
	_ = m.Cells.CloseMedia(s.cell, s.id)
	_ = s.leg.Close()
}

// MediaOpened is the cell's answer to MEDIA_OPEN (cell.MediaSink).
func (m *Manager) MediaOpened(_ uuid.UUID, a wire.MediaOpenAckMsg) {
	m.mu.Lock()
	ch := m.pending[a.MediaID]
	m.mu.Unlock()
	if ch != nil {
		select {
		case ch <- a:
		default:
		}
	}
}

// MediaClosed is a flow the cell ended (cell.MediaSink): the call goes on
// silent, ending it is the handset's or the far end's business.
func (m *Manager) MediaClosed(_ uuid.UUID, c wire.MediaClosedMsg) {
	m.mu.Lock()
	s := m.sessions[c.MediaID]
	m.mu.Unlock()
	if s != nil && c.Reason != wire.MediaClosedRequested {
		m.log().Info("the cell ended a call's speech", "media", c.MediaID, "reason", c.Reason)
		s.cancel()
	}
}
