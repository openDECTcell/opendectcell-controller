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
	sipCodec, err := codec.ByName(neg.Codec.Name)
	if err != nil {
		return nil, err
	}
	f, err := m.openFlow(ctx, ev, sipCodec)
	if err != nil {
		return nil, err
	}
	cfg := media.Config{Plan: f.plan, SIP: media.SIPLeg(c.Media()), SIPPayloadType: neg.Codec.PT,
		SIPPtime: sipPtime(neg.PTime), Cell: f.leg, CellPayload: cellPayloadType(f.plan), Log: m.log()}
	if neg.TelEventPT != 0 {
		cfg.Events = c.Media()
	}
	return m.startSession(ctx, ev, f, cfg)
}

// cellFlow is the controller's end of one cell's speech once the cell has taken MEDIA_OPEN.
type cellFlow struct {
	id   uint32
	plan media.Plan
	leg  *media.CellLeg
}

// openFlow ties a cell's audio channel to a UDP port here (MEDIA_OPEN, SRTP) for a call whose other
// side speaks sipCodec, and waits for the cell to accept.
func (m *Manager) openFlow(ctx context.Context, ev core.CallEvent, sipCodec *codec.Codec) (*cellFlow, error) {
	st := m.Cells.CellStatus(ev.Cell)
	ip := m.MediaIP
	if m.MediaFromLink && st.LocalAddr.IsValid() {
		ip = st.LocalAddr
	}
	if !ip.IsValid() {
		return nil, errNoMediaIP
	}
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
		m.pending, m.sessions = map[uint32]pendingOpen{}, map[uint32]*mediaSession{}
	}
	ack := make(chan wire.MediaOpenAckMsg, 1)
	m.pending[id] = pendingOpen{cell: ev.Cell, ch: ack}
	m.mu.Unlock()
	drop := func() {
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
	}
	mo, err := media.NewMediaOpen(id, ev.Channel, plan, cellPayloadType(plan), netip.AddrPortFrom(ip, port), wire.SRTPAES128SHA80)
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
	return &cellFlow{id: id, plan: plan, leg: leg}, nil
}

// startSession runs the pipeline of an open flow. cfg is complete but for the media's id: a pipeline
// that cannot be made closes the flow.
func (m *Manager) startSession(ctx context.Context, ev core.CallEvent, f *cellFlow, cfg media.Config) (*mediaSession, error) {
	pipe, err := media.New(cfg)
	if err != nil {
		_ = m.Cells.CloseMedia(ev.Cell, f.id)
		f.leg.Close()
		return nil, err
	}
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &mediaSession{id: f.id, cell: ev.Cell, plan: f.plan, leg: f.leg, pipe: pipe, cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.sessions[f.id] = s
	m.mu.Unlock()
	go func() {
		defer close(s.done)
		if err := pipe.Run(rctx); err != nil && !errors.Is(err, context.Canceled) {
			m.log().Info("a call's media pipeline ended", "err", err)
		}
	}()
	m.log().Info("a call's speech is open", "cell", ev.Cell, "media", f.id, "plan", f.plan.String(), "channel", ev.Channel)
	return s, nil
}

// dropFlow gives up a flow that no pipeline was made for.
func (m *Manager) dropFlow(ev core.CallEvent, f *cellFlow) {
	_ = m.Cells.CloseMedia(ev.Cell, f.id)
	f.leg.Close()
}

// sipPtime is the packet time the SIP side negotiated, or 0 (20 ms) for one the
// media pipeline cannot run on (it clocks 10, 20, 30 or 40 ms): the call then
// carries speech at the usual 20 ms rather than none.
func sipPtime(ms int) time.Duration {
	switch ms {
	case 10, 20, 30, 40:
		return time.Duration(ms) * time.Millisecond
	}
	return 0
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

// pendingOpen is a MEDIA_OPEN sent and not yet answered: the cell it went to, which alone may answer it
// (media ids are a counter, so another cell could guess one), and where the answer goes.
type pendingOpen struct {
	cell uuid.UUID
	ch   chan wire.MediaOpenAckMsg
}

// MediaOpened is the cell's answer to MEDIA_OPEN (cell.MediaSink). An answer from a cell the request did
// not go to is dropped.
func (m *Manager) MediaOpened(cell uuid.UUID, a wire.MediaOpenAckMsg) {
	m.mu.Lock()
	p := m.pending[a.MediaID]
	m.mu.Unlock()
	if p.ch == nil || p.cell != cell {
		return
	}
	select {
	case p.ch <- a:
	default:
	}
}

// MediaClosed is a flow the cell ended (cell.MediaSink): the call goes on
// silent, ending it is the handset's or the far end's business.
func (m *Manager) MediaClosed(cell uuid.UUID, c wire.MediaClosedMsg) {
	m.mu.Lock()
	s := m.sessions[c.MediaID]
	m.mu.Unlock()
	if s != nil && s.cell == cell && c.Reason != wire.MediaClosedRequested {
		m.log().Info("the cell ended a call's speech", "media", c.MediaID, "reason", c.Reason)
		s.cancel()
	}
}
