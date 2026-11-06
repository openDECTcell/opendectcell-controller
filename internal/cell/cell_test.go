// SPDX-License-Identifier: AGPL-3.0-or-later

package cell_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/celltest"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

type env struct {
	pool  *pgxpool.Pool
	store *cell.Store
	srv   *cell.Server
	admin *auth.User
	users *auth.Store
	ctx   context.Context
}

func newEnv(t *testing.T) *env { return newEnvOn(t, func(ln net.Listener) net.Listener { return ln }) }

// newEnvOn is newEnv with the listener the server accepts on wrapped (to make accepting fail).
func newEnvOn(t *testing.T, wrap func(net.Listener) net.Listener) *env {
	return newEnvTuned(t, wrap, nil)
}

// newEnvTuned is newEnvOn with a last look at the server's settings before it starts.
func newEnvTuned(t *testing.T, wrap func(net.Listener) net.Listener, tune func(*cell.Server)) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pool := dbtest.Migrated(t)
	ks, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(ks)
	kr, err := db.LoadKeyring(ctx, pool, mk, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	st, err := cell.Open(ctx, pool, kr, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv := &cell.Server{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Heartbeat: 50 * time.Millisecond, Dead: 300 * time.Millisecond, Grace: 400 * time.Millisecond,
		Orphan: 2 * time.Second, EnrollDelay: 10 * time.Millisecond, Recheck: 100 * time.Millisecond}
	if tune != nil {
		tune(srv)
	}
	done := make(chan struct{})
	go func() { srv.Serve(ctx, wrap(ln)); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	users := &auth.Store{Pool: pool, Argon: auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1}, SessionIdle: time.Hour, SessionMaxAge: time.Hour, RateLimit: 5}
	admin, err := users.CreateLocalUser(ctx, auth.NewLocalUser{Username: "admin", Password: "admin password", Role: auth.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	return &env{pool: pool, store: st, srv: srv, admin: admin, users: users, ctx: audit.WithActor(ctx, admin.Actor())}
}

// token makes an enrollment token for u. CreateToken's tokens approve the cell by themselves; a token that
// does not stands for one made before that (such a cell enrolls as pending).
func (e *env) token(t *testing.T, u *auth.User, note string, ttl time.Duration, auto bool) (*cell.Token, string, error) {
	t.Helper()
	tok, es, err := e.store.CreateToken(e.ctx, u, note, ttl)
	if err == nil && !auto {
		if _, err := e.pool.Exec(e.ctx, "UPDATE cell_enrollment_token SET auto_approve = false WHERE id = $1", tok.ID); err != nil {
			t.Fatal(err)
		}
	}
	return tok, es, err
}

func (e *env) enrolled(t *testing.T, auto bool) *celltest.Cell {
	t.Helper()
	_, es, err := e.token(t, e.admin, "test", 0, auto)
	if err != nil {
		t.Fatal(err)
	}
	c, status, err := celltest.Enroll(e.ctx, es, "box")
	if err != nil || status != wire.EnrollOK {
		t.Fatalf("enroll: %d %v", status, err)
	}
	return c
}

func expectState(t *testing.T, cn *celltest.Conn, want wire.CellStateValue) {
	t.Helper()
	f, err := cn.Expect(wire.CellState, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := wire.DecodeCellState(f.Body)
	if m.State != want {
		t.Fatalf("CELL_STATE %s, want %s", m.State, want)
	}
}

func TestEnrollment(t *testing.T) {
	e := newEnv(t)
	_, es, err := e.token(t, e.admin, "Garage", time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(es, "opendectcell-enroll:v1?h=") {
		t.Fatalf("enrollment string %q", es)
	}
	c, status, err := celltest.Enroll(e.ctx, es, "")
	if err != nil || status != wire.EnrollOK || c.State != wire.StatePending {
		t.Fatalf("enroll: %d %v %+v", status, err, c)
	}
	got, err := e.store.GetCell(e.ctx, e.admin, c.ID)
	if err != nil || got.Name != "Garage" || got.State != "pending" {
		t.Fatalf("cell row: %+v %v", got, err)
	}
	// Single use: the same string again is rejected, indistinguishably
	// from an unknown token.
	if _, status, _ := celltest.Enroll(e.ctx, es, ""); status != wire.EnrollTokenRejected {
		t.Errorf("reused token: %d", status)
	}
	// Auto-approve gives an active cell; names are made unique.
	_, es2, _ := e.token(t, e.admin, "Garage", time.Hour, true)
	c2, _, _ := celltest.Enroll(e.ctx, es2, "")
	got2, _ := e.store.GetCell(e.ctx, e.admin, c2.ID)
	if c2.State != wire.StateActive || got2.Name != "Garage-2" {
		t.Errorf("auto-approved: %s %q", c2.State, got2.Name)
	}

	// Rate limit: 5 rejected tokens from one address, then RATE_LIMITED.
	// The reused token above was the first.
	for i := range 4 {
		if st, err := c.EnrollWithToken(e.ctx, make([]byte, 20)); st != wire.EnrollTokenRejected {
			t.Fatalf("attempt %d: %d %v", i, st, err)
		}
	}
	if st, _ := c.EnrollWithToken(e.ctx, make([]byte, 20)); st != wire.EnrollRateLimited {
		t.Errorf("after 5 failures: %d", st)
	}
	rows, _ := audit.List(e.ctx, e.store.Pool, audit.Filter{Limit: 100})
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Action]++
		if r.Action == "cell.enroll_failed" && r.RemoteAddr == nil {
			t.Error("failed enrollment without source address")
		}
	}
	if counts["cell.enroll"] != 2 || counts["cell.enroll_failed"] != 5 || counts["cell.enroll_ratelimited"] != 1 {
		t.Errorf("audit: %v", counts)
	}
}

func TestTokenAuthorisation(t *testing.T) {
	e := newEnv(t)
	u, _ := e.users.CreateLocalUser(e.ctx, auth.NewLocalUser{Username: "u", Password: "user password", Role: auth.RoleUser})
	if _, _, err := e.token(t, u, "", 0, false); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("user without may_add_cells: %v", err)
	}
	u.MayAddCells = true
	mine, _, err := e.token(t, u, "mine", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	adm, _, _ := e.token(t, e.admin, "admin's", 0, false)
	list, _ := e.store.ListTokens(e.ctx, u)
	if len(list) != 1 || list[0].ID != mine.ID {
		t.Fatalf("user sees %d tokens", len(list))
	}
	if all, _ := e.store.ListTokens(e.ctx, e.admin); len(all) != 2 {
		t.Fatalf("admin sees %d tokens", len(all))
	}
	if err := e.store.DeleteToken(e.ctx, u, adm.ID); !errors.Is(err, cell.ErrNotFound) {
		t.Fatalf("user deleted admin's token: %v", err)
	}
	if err := e.store.DeleteToken(e.ctx, u, mine.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.token(t, e.admin, "", time.Minute, false); err == nil {
		t.Error("1-minute lifetime accepted")
	}
	// Users see active cells, reduced to id, name and location.
	c := e.enrolled(t, true)
	cells, _ := e.store.ListCells(e.ctx, u)
	if len(cells) != 1 || cells[0].ID != c.ID || cells[0].EnrolledAt != nil {
		t.Fatalf("user cell list: %+v", cells)
	}
	if _, err := e.store.GetCell(e.ctx, u, c.ID); !errors.Is(err, cell.ErrNotFound) {
		t.Errorf("user got cell details: %v", err)
	}
}

// The owner revokes their own base station from "Base stations"; nobody
// else's. A plain Revoke stays admin-only.
func TestRevokeOwned(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.users.CreateLocalUser(e.ctx, auth.NewLocalUser{Username: "owner", Password: "user password", Role: auth.RoleUser})
	other, _ := e.users.CreateLocalUser(e.ctx, auth.NewLocalUser{Username: "other", Password: "user password", Role: auth.RoleUser})
	owner.MayAddCells, other.MayAddCells = true, true
	e.pool.Exec(e.ctx, "UPDATE app_user SET may_add_cells = true") // a token is honoured only while its creator may enroll
	_, es, err := e.token(t, owner, "mine", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	c, status, err := celltest.Enroll(e.ctx, es, "box")
	if err != nil || status != wire.EnrollOK {
		t.Fatalf("enroll: %d %v", status, err)
	}
	if err := e.store.RevokeOwned(e.ctx, other, c.ID); !errors.Is(err, cell.ErrNotFound) {
		t.Fatalf("somebody else revoked it: %v", err)
	}
	if err := e.store.Revoke(e.ctx, owner, c.ID); !errors.Is(err, cell.ErrNotFound) {
		t.Fatalf("the plain Revoke let the owner in: %v", err)
	}
	if err := e.store.RevokeOwned(e.ctx, owner, c.ID); err != nil {
		t.Fatalf("the owner could not revoke: %v", err)
	}
	if got, err := e.store.GetOwned(e.ctx, owner, c.ID); err != nil || got.State != "revoked" {
		t.Fatalf("after revoke: %+v %v", got, err)
	}
}

// A revoked cell is deleted three days after it was revoked; not before, and nothing else is.
func TestPurgeRevoked(t *testing.T) {
	e := newEnv(t)
	admin, _ := e.users.CreateLocalUser(e.ctx, auth.NewLocalUser{Username: "boss", Password: "admin password", Role: auth.RoleAdmin})
	keep, gone := e.enrolled(t, true), e.enrolled(t, true)
	if err := e.store.Revoke(e.ctx, admin, gone.ID); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	e.store.Now = func() time.Time { return t0.Add(71 * time.Hour) }
	if n, err := e.store.PurgeRevoked(e.ctx); err != nil || n != 0 {
		t.Fatalf("after 71 h: deleted %d, %v", n, err)
	}
	e.store.Now = func() time.Time { return t0.Add(73 * time.Hour) }
	if n, err := e.store.PurgeRevoked(e.ctx); err != nil || n != 1 {
		t.Fatalf("after 73 h: deleted %d, %v", n, err)
	}
	cells, err := e.store.ListCells(e.ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cells {
		if c.ID == gone.ID {
			t.Errorf("the revoked cell is still listed")
		}
	}
	if len(cells) != 1 || cells[0].ID != keep.ID {
		t.Errorf("the other cell was touched: %d cells", len(cells))
	}
}

// A cell that announces media and SRTP is granted both: without the grant it
// refuses MEDIA_OPEN as "not allowed" (found on air, 2026-11-01). Plain RTP is
// never granted.
func TestHelloAckGrantsMedia(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, true)
	cn, err := c.Connect(e.ctx, celltest.Options{Media: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	got := cn.Ack.Capabilities
	if got&wire.CapMedia == 0 || got&wire.CapSRTP == 0 || got&wire.CapPlainRTP != 0 {
		t.Errorf("capabilities %b", got)
	}
}

func TestHandshakeApproveAndHeartbeat(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, false)
	cn, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	if cn.Ack.CellID != c.ID || cn.Ack.State != wire.StatePending || cn.Ack.Resumed || cn.Ack.HeartbeatMs != 50 {
		t.Fatalf("HELLO_ACK: %+v", cn.Ack)
	}
	if cn.Ack.Capabilities&wire.CapMail == 0 || cn.Ack.Capabilities&wire.CapMedia != 0 {
		t.Errorf("capabilities %b", cn.Ack.Capabilities)
	}
	expectState(t, cn, wire.StatePending)

	// A pending cell may not use the radio.
	cn.Send(wire.Frame{Type: wire.MailUp, Corr: 5, Body: make([]byte, 9)})
	f, err := cn.Expect(wire.Error, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := wire.DecodeError(f.Body); m.Code != wire.ErrNotAllowed || m.RefCorr != 5 {
		t.Errorf("MAIL_UP while pending: %+v", m)
	}
	// Unknown types are answered and the link stays up.
	cn.Send(wire.Frame{Type: wire.Type(0x0777), Corr: 6})
	f, _ = cn.Expect(wire.Error, time.Second)
	if m, _ := wire.DecodeError(f.Body); m.Code != wire.ErrUnknownType || m.RefType != 0x0777 {
		t.Errorf("unknown type: %+v", m)
	}

	// Approval is pushed to the connected cell.
	st, err := e.store.SetState(e.ctx, e.admin, c.ID, "active")
	if err != nil {
		t.Fatal(err)
	}
	e.srv.NotifyState(c.ID, st, "approved")
	expectState(t, cn, wire.StateActive)

	// Heartbeats both ways: the cell's is echoed; the controller measures RTT.
	cn.Send(wire.Frame{Type: wire.Heartbeat, Body: wire.HeartbeatBody(77)})
	f, err = cn.Expect(wire.HeartbeatEcho, time.Second)
	if v, _ := wire.DecodeHeartbeat(f.Body); err != nil || v != 77 {
		t.Fatalf("echo: %v %v", v, err)
	}
	cn.Send(wire.Frame{Type: wire.Stats, Body: wire.EncodeStats(map[uint16]uint64{1: 12, 19: 0})})
	deadline := time.Now().Add(2 * time.Second)
	for {
		s := e.srv.CellStatus(c.ID)
		if s.Online && s.RTTp50 > 0 && s.Stats["mails_up"] == 12 {
			if s.Health() != "ok" {
				t.Errorf("health %s", s.Health())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status: %+v", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Mails from an active cell with no sink are counted and dropped.
	cn.Send(wire.Frame{Type: wire.MailUp, Body: make([]byte, 9)})
	for end := time.Now().Add(3 * time.Second); e.srv.CellStatus(c.ID).MailsDrop != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("mail from an active cell not counted")
		}
	}
}

// mailSink records the tunnel's far end.
type mailSink struct {
	up  chan wire.MailUpMsg
	rej chan wire.MailRejectMsg
}

func (m *mailSink) MailUp(_ uuid.UUID, u wire.MailUpMsg) {
	u.Param = append([]byte(nil), u.Param...)
	m.up <- u
}

func (m *mailSink) MailReject(_ uuid.UUID, r wire.MailRejectMsg) { m.rej <- r }

type mediaSink struct {
	opened chan wire.MediaOpenAckMsg
	closed chan wire.MediaClosedMsg
}

func (m *mediaSink) MediaOpened(_ uuid.UUID, a wire.MediaOpenAckMsg) { m.opened <- a }
func (m *mediaSink) MediaClosed(_ uuid.UUID, c wire.MediaClosedMsg)  { m.closed <- c }

// MEDIA_OPEN and MEDIA_CLOSE go to an active cell; its answers and a flow it
// ends come back to Server.Media.
func TestMediaMessages(t *testing.T) {
	e := newEnv(t)
	sink := &mediaSink{opened: make(chan wire.MediaOpenAckMsg, 4), closed: make(chan wire.MediaClosedMsg, 4)}
	e.srv.Media = sink
	c := e.enrolled(t, true)
	cn, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	expectState(t, cn, wire.StateActive)

	mo := wire.MediaOpenMsg{MediaID: 9, AudioChannel: 1, Format: wire.FormatL16_8k, PayloadType: 96,
		CtlAddr: netip.MustParseAddrPort("192.0.2.1:41100"), SSRCUp: 1, SSRCDown: 2, SRTPProfile: wire.SRTPAES128SHA80,
		KeyUp: make([]byte, 30), KeyDown: make([]byte, 30)}
	if err := e.srv.OpenMedia(c.ID, mo); err != nil {
		t.Fatal(err)
	}
	f, err := cn.Expect(wire.MediaOpen, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := wire.DecodeMediaOpen(f.Body); err != nil || got.MediaID != 9 || got.CtlAddr != mo.CtlAddr || got.AudioChannel != 1 {
		t.Errorf("MEDIA_OPEN %+v %v", got, err)
	}
	cn.Send(wire.Frame{Type: wire.MediaOpenAck, Body: wire.MediaOpenAckMsg{MediaID: 9, Status: wire.MediaOK}.Encode()})
	select {
	case a := <-sink.opened:
		if a.MediaID != 9 || a.Status != wire.MediaOK {
			t.Errorf("ack %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no MEDIA_OPEN_ACK at the sink")
	}
	if err := e.srv.CloseMedia(c.ID, 9); err != nil {
		t.Fatal(err)
	}
	f, err = cn.Expect(wire.MediaClose, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := wire.DecodeMediaClose(f.Body); err != nil || id != 9 {
		t.Errorf("MEDIA_CLOSE %d %v", id, err)
	}
	cn.Send(wire.Frame{Type: wire.MediaClosed, Body: wire.MediaClosedMsg{MediaID: 9, Reason: wire.MediaClosedRequested}.Encode()})
	select {
	case m := <-sink.closed:
		if m.MediaID != 9 || m.Reason != wire.MediaClosedRequested {
			t.Errorf("closed %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no MEDIA_CLOSED at the sink")
	}
	// an ERROR the cell answers MEDIA_OPEN with is an answer, not silence
	if err := e.srv.OpenMedia(c.ID, wire.MediaOpenMsg{MediaID: 11, AudioChannel: 1, Format: wire.FormatL16_8k, PayloadType: 96,
		CtlAddr: mo.CtlAddr, SRTPProfile: wire.SRTPNone}); err != nil {
		t.Fatal(err)
	}
	f, err = cn.Expect(wire.MediaOpen, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cn.Send(wire.Frame{Type: wire.Error, Corr: f.Corr, Body: wire.ErrorMsg{Code: wire.ErrNotAllowed, RefType: wire.MediaOpen, RefCorr: f.Corr}.Encode()})
	select {
	case a := <-sink.opened:
		if a.MediaID != 11 || a.Status == wire.MediaOK {
			t.Errorf("refusal %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a refused MEDIA_OPEN did not reach the sink")
	}
	// a malformed answer is an ERROR
	cn.Send(wire.Frame{Type: wire.MediaOpenAck, Body: []byte{1, 2}})
	if _, err := cn.Expect(wire.Error, 2*time.Second); err != nil {
		t.Error("no ERROR for a malformed MEDIA_OPEN_ACK")
	}
	// no session: no media
	if err := e.srv.OpenMedia(uuid.New(), mo); !errors.Is(err, cell.ErrNoCell) {
		t.Errorf("OpenMedia to an unknown cell: %v", err)
	}
}

func TestMailTunnel(t *testing.T) {
	e := newEnv(t)
	sink := &mailSink{up: make(chan wire.MailUpMsg, 4), rej: make(chan wire.MailRejectMsg, 4)}
	e.srv.Mails = sink
	c := e.enrolled(t, true)
	cn, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	expectState(t, cn, wire.StateActive)

	// up: the sink gets the mail as the cell sent it
	want := wire.MailUpMsg{RxTime: 42, Src: 0x02, Dst: 0x14, Prim: 0x0200, Param: []byte{1, 2, 3}}
	cn.Send(wire.Frame{Type: wire.MailUp, Body: want.Encode()})
	select {
	case got := <-sink.up:
		if got.RxTime != 42 || got.Prim != 0x0200 || string(got.Param) != "\x01\x02\x03" {
			t.Errorf("mail up: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no MAIL_UP at the sink")
	}
	// down: queued to the cell with its corr
	if err := e.srv.SendMail(c.ID, 7, wire.MailDownMsg{Src: 0x22, Dst: 0x02, Prim: 0x0244, Param: []byte{0, 1, 9}}); err != nil {
		t.Fatal(err)
	}
	f, err := cn.Expect(wire.MailDown, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := wire.DecodeMailDown(f.Body); err != nil || f.Corr != 7 || d.Prim != 0x0244 || len(d.Param) != 3 {
		t.Errorf("mail down: %+v corr %d %v", d, f.Corr, err)
	}
	// reject: to the sink
	cn.Send(wire.Frame{Type: wire.MailReject, Body: wire.MailRejectMsg{RefCorr: 7, Reason: wire.RejectQueueFull}.Encode()})
	select {
	case r := <-sink.rej:
		if r.RefCorr != 7 || r.Reason != wire.RejectQueueFull {
			t.Errorf("reject: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no MAIL_REJECT at the sink")
	}
	// a malformed mail is an ERROR, not a sink call
	cn.Send(wire.Frame{Type: wire.MailUp, Body: []byte{0, 0, 0, 0, 2, 0x14, 2, 0, 5}})
	if _, err := cn.Expect(wire.Error, 2*time.Second); err != nil {
		t.Error("no ERROR for a short MAIL_UP")
	}
	st := e.srv.CellStatus(c.ID)
	if st.MailsUp != 1 || st.MailsDown != 1 || st.MailsReject != 1 {
		t.Errorf("counters: %+v", st)
	}
	// no session: no mail
	if err := e.srv.SendMail(uuid.New(), 0, wire.MailDownMsg{}); !errors.Is(err, cell.ErrNoCell) {
		t.Errorf("SendMail to an unknown cell: %v", err)
	}
}

// watchSink is a mail sink that is also a CellWatcher.
type watchSink struct {
	mailSink
	ev chan string
}

func (w *watchSink) CellUp(_ uuid.UUID, up cell.CellUp) {
	w.ev <- fmt.Sprintf("up resumed=%v", up.Resumed)
}
func (w *watchSink) CellDown(uuid.UUID) { w.ev <- "down" }
func (w *watchSink) CellGone(uuid.UUID) { w.ev <- "gone" }

func TestCellWatcher(t *testing.T) {
	e := newEnv(t)
	w := &watchSink{ev: make(chan string, 16)}
	e.srv.Mails = w
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-w.ev:
			if got != want {
				t.Fatalf("watcher: %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("watcher: nothing, want %q", want)
		}
	}
	c := e.enrolled(t, true)
	cn, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	expect("up resumed=false")
	tok := cn.Ack.SessionToken
	cn.Close()
	expect("down")
	cn2, err := c.Connect(e.ctx, celltest.Options{ResumeToken: tok[:]})
	if err != nil {
		t.Fatal(err)
	}
	expect("up resumed=true")
	cn2.Close()
	expect("down")
	expect("gone") // the grace period (400 ms) ran out
}

func TestDeadLinkAndResume(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, true)
	cn, err := c.Connect(e.ctx, celltest.Options{NoAutoEcho: true})
	if err != nil {
		t.Fatal(err)
	}
	tok := cn.Ack.SessionToken
	// The cell sends nothing, not even echoes: the controller closes it
	// after dead_ms.
	start := time.Now()
	if _, err := cn.WaitClosed(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("closed after %v, before dead_ms", d)
	}
	// Within the grace period, the token resumes.
	cn2, err := c.Connect(e.ctx, celltest.Options{ResumeToken: tok[:]})
	if err != nil {
		t.Fatal(err)
	}
	if !cn2.Ack.Resumed || cn2.Ack.SessionToken == tok {
		t.Fatalf("resume: %+v", cn2.Ack)
	}
	cn2.Close()
	// An old token does not resume; after the grace period nothing does.
	time.Sleep(50 * time.Millisecond)
	cn3, _ := c.Connect(e.ctx, celltest.Options{ResumeToken: tok[:]})
	if cn3.Ack.Resumed {
		t.Error("stale token resumed")
	}
	tok3 := cn3.Ack.SessionToken
	cn3.Close()
	time.Sleep(800 * time.Millisecond) // grace is 400 ms; margin for slow CI
	cn4, _ := c.Connect(e.ctx, celltest.Options{ResumeToken: tok3[:]})
	defer cn4.Close()
	if cn4.Ack.Resumed {
		t.Error("resumed after the grace period")
	}
	// A changed chip epoch is a fresh start even inside the grace period.
	tok4 := cn4.Ack.SessionToken
	cn4.Close()
	cn5, _ := c.Connect(e.ctx, celltest.Options{ResumeToken: tok4[:], ChipEpoch: 9})
	defer cn5.Close()
	if cn5.Ack.Resumed {
		t.Error("resumed across a chip restart")
	}
}

func TestSupersedeRevokeVersion(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, true)
	first, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	bye, err := first.WaitClosed(2 * time.Second)
	if err != nil || bye == nil || bye.Reason != wire.ByeSuperseded {
		t.Fatalf("first connection: %+v %v", bye, err)
	}
	if s := e.srv.CellStatus(c.ID); !s.Online || s.Supersedes != 1 {
		t.Fatalf("after supersede: %+v", s)
	}

	if err := e.store.Revoke(e.ctx, e.admin, c.ID); err != nil {
		t.Fatal(err)
	}
	e.srv.Kick(c.ID)
	bye, _ = second.WaitClosed(2 * time.Second)
	if bye == nil || bye.Reason != wire.ByeRevoked {
		t.Fatalf("revoked link: %+v", bye)
	}
	_, err = c.Connect(e.ctx, celltest.Options{})
	var ge *celltest.GoodbyeError
	if !errors.As(err, &ge) || ge.Msg.Reason != wire.ByeRevoked {
		t.Fatalf("reconnect after revoke: %v", err)
	}

	c2 := e.enrolled(t, true)
	_, err = c2.Connect(e.ctx, celltest.Options{Major: 2})
	if !errors.As(err, &ge) || ge.Msg.Reason != wire.ByeVersion {
		t.Fatalf("major 2: %v", err)
	}
}

func TestRenewal(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, true)
	cn, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	old := *c
	if err := c.Renew(cn); err != nil {
		t.Fatal(err)
	}
	cn.Close()
	if c.Cert.SerialNumber.Cmp(old.Cert.SerialNumber) == 0 {
		t.Fatal("same certificate after renewal")
	}
	// Until the cell connects with the new one, both are current.
	if cn, err := old.Connect(e.ctx, celltest.Options{}); err != nil {
		t.Fatalf("old certificate before switching: %v", err)
	} else {
		cn.Close()
	}
	cn2, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatalf("new certificate: %v", err)
	}
	cn2.Close()
	var ge *celltest.GoodbyeError
	if _, err := old.Connect(e.ctx, celltest.Options{}); !errors.As(err, &ge) || ge.Msg.Reason != wire.ByeAuth {
		t.Fatalf("old certificate after the switch: %v", err)
	}
}

func TestForeignCertificateRejected(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, true)
	other := newEnv(t)
	o := other.enrolled(t, true)
	// A cell of another installation, presenting its certificate here.
	o.Addr, o.Pins = c.Addr, c.Pins
	if _, err := o.Connect(e.ctx, celltest.Options{}); err == nil {
		t.Fatal("certificate from another CA accepted")
	}
}

// The RFPI in HELLO shows in the cell's live status at once: a cell that sends
// no STATUS after its HELLO still has its identity on the cells page.
func TestHelloRFPIShowsInTheStatus(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, false)
	rfpi := [5]byte{0x03, 0x69, 0x0e, 0x24, 0xb8}
	cn, err := c.Connect(e.ctx, celltest.Options{RFPI: &rfpi})
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if s := e.srv.CellStatus(c.ID); s.HasRFPI {
			if s.RFPI != rfpi {
				t.Fatalf("status RFPI %x, want %x", s.RFPI, rfpi)
			}
			return
		}
		if time.Now().After(end) {
			t.Fatal("the HELLO's RFPI never reached the live status")
		}
	}
}

// Connections that never authenticate cannot pile up: one source gets
// DefaultMaxPreAuthPerAddr of them and the next is closed at once; when they
// end, a cell connects again.
func TestPreAuthFloodDoesNotBlockACell(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, true)
	var idle []net.Conn
	for range cell.DefaultMaxPreAuthPerAddr {
		nc, err := net.Dial("tcp", c.Addr)
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, nc)
	}
	t.Cleanup(func() {
		for _, nc := range idle {
			nc.Close()
		}
	})
	// The listener has taken them all once its count shows it.
	deadline := time.Now().Add(3 * time.Second)
	for e.srv.PreAuthCount() < cell.DefaultMaxPreAuthPerAddr {
		if time.Now().After(deadline) {
			t.Fatalf("only %d connections counted", e.srv.PreAuthCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
	extra, err := net.Dial("tcp", c.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the connection over the limit was kept open: %v", err)
	}
	// The same source cannot connect while the idle ones hold its places ...
	if cn, err := c.Connect(e.ctx, celltest.Options{}); err == nil {
		cn.Close()
		t.Fatal("a connection over the per-source limit was served")
	}
	// ... and can again once they are gone.
	for _, nc := range idle {
		nc.Close()
	}
	idle = nil
	for e.srv.PreAuthCount() > 0 {
		if time.Now().After(deadline.Add(10 * time.Second)) {
			t.Fatalf("%d connections still counted", e.srv.PreAuthCount())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cn, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatalf("cell could not connect after the flood: %v", err)
	}
	cn.Close()
}

// A revocation reaches a connected cell even if nobody calls Kick (the message
// was lost, or the controller restarted between the change and the kick): the
// session looks its cell up again and closes.
func TestRevocationIsRecheckedWithoutKick(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, true)
	cn, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	if err := e.store.Revoke(e.ctx, e.admin, c.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var sawGoodbye bool
	for time.Now().Before(deadline) {
		f, err := cn.Next(time.Second)
		if err != nil {
			if !sawGoodbye {
				t.Log("link closed without a GOODBYE (acceptable); error:", err)
			}
			return
		}
		if f.Type == wire.Goodbye {
			sawGoodbye = true
			if m, err := wire.DecodeGoodbye(f.Body); err != nil || m.Reason != wire.ByeRevoked {
				t.Fatalf("goodbye %+v %v", m, err)
			}
		}
	}
	t.Fatal("a revoked cell stayed connected")
}

// After Kick the session handles nothing more, even from a cell that keeps
// sending.
func TestKickEndsTheLink(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, true)
	cn, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	e.srv.Kick(c.ID)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := cn.Next(time.Second); err != nil {
			return
		}
	}
	t.Fatal("the link stayed open after Kick")
}

// A user who may enroll cells needs no administrator to approve the cell: it is active when it enrolls.
func TestUserEnrolledCellIsActive(t *testing.T) {
	e := newEnv(t)
	u, _ := e.users.CreateLocalUser(e.ctx, auth.NewLocalUser{Username: "u", Password: "user password", Role: auth.RoleUser, MayAddCells: true})
	tok, es, err := e.store.CreateToken(e.ctx, u, "Attic", time.Hour)
	if err != nil || !tok.AutoApprove {
		t.Fatalf("token: %+v %v", tok, err)
	}
	c, status, err := celltest.Enroll(e.ctx, es, "")
	if err != nil || status != wire.EnrollOK || c.State != wire.StateActive {
		t.Fatalf("enroll: %d %v %+v", status, err, c)
	}
	got, err := e.store.GetCell(e.ctx, e.admin, c.ID)
	if err != nil || got.State != "active" || got.EnrolledBy == nil || *got.EnrolledBy != u.ID {
		t.Fatalf("cell row: %+v %v", got, err)
	}
}

// A user sees of another operator's base station only what that operator shares (PRIV-01).
func TestListCellsRespectsSharing(t *testing.T) {
	e := newEnv(t)
	u, _ := e.users.CreateLocalUser(e.ctx, auth.NewLocalUser{Username: "u", Password: "user password", Role: auth.RoleUser})
	o, _ := e.users.CreateLocalUser(e.ctx, auth.NewLocalUser{Username: "o", Password: "other password", Role: auth.RoleUser})
	ins := func(name string, by *auth.User, shareName, sharePos bool) {
		if _, err := e.pool.Exec(e.ctx, `INSERT INTO cell (name, location, state, enrolled_by, share_name, share_position)
			VALUES ($1, $1 || ' place', 'active', $2, $3, $4)`, name, by.ID, shareName, sharePos); err != nil {
			t.Fatal(err)
		}
	}
	ins("Mine", u, false, false)
	ins("Private", o, false, false)
	ins("NamedOnly", o, true, false)
	ins("Shared", o, true, true)
	cells, err := e.store.ListCells(e.ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]string{}
	for _, c := range cells {
		got[c.Name] = [2]string{c.Name, c.Location}
		if c.EnrolledBy != nil || c.SoftwareVersion != nil {
			t.Errorf("a user's list carries operator details: %+v", c)
		}
	}
	if len(cells) != 4 {
		t.Fatalf("%d cells", len(cells))
	}
	if got["Mine"] != [2]string{"Mine", "Mine place"} {
		t.Errorf("own base station: %v", got["Mine"])
	}
	if _, ok := got["Private"]; ok {
		t.Error("the name of a base station that does not share its name is listed")
	}
	if got["NamedOnly"] != [2]string{"NamedOnly", ""} {
		t.Errorf("name shared, position not: %v", got["NamedOnly"])
	}
	if got["Shared"] != [2]string{"Shared", "Shared place"} {
		t.Errorf("shared: %v", got["Shared"])
	}
}

// A token is good only while its creator may still enroll cells, and a user holds a limited number of them.
func TestTokenFollowsItsCreator(t *testing.T) {
	e := newEnv(t)
	mk := func(name string) *auth.User {
		u, err := e.users.CreateLocalUser(e.ctx, auth.NewLocalUser{Username: name, Password: "user password", Role: auth.RoleUser, MayAddCells: true})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	enrol := func(es string) uint16 {
		_, status, _ := celltest.Enroll(e.ctx, es, "")
		return status
	}

	// the permission is taken away
	u := mk("u1")
	_, es, _ := e.store.CreateToken(e.ctx, u, "", time.Hour)
	e.pool.Exec(e.ctx, "UPDATE app_user SET may_add_cells = false WHERE id = $1", u.ID)
	if st := enrol(es); st != wire.EnrollTokenRejected {
		t.Errorf("a token of a user who may not enroll any more: %d", st)
	}
	// ...but not if the installation lets everyone
	_, es, _ = e.store.CreateToken(e.ctx, mk("u2"), "", time.Hour)
	e.pool.Exec(e.ctx, "UPDATE app_user SET may_add_cells = false WHERE username = 'u2'")
	e.store.EveryoneMayEnroll = func(context.Context) bool { return true }
	if st := enrol(es); st != wire.EnrollOK {
		t.Errorf("a token while everyone may enroll: %d", st)
	}
	e.store.EveryoneMayEnroll = nil

	// the user is disabled by an administrator: the token goes
	u3 := mk("u3")
	_, es, _ = e.store.CreateToken(e.ctx, u3, "", time.Hour)
	off := true
	if _, err := e.users.UpdateUser(e.ctx, e.admin, u3.ID, auth.UserUpdate{Disabled: &off}); err != nil {
		t.Fatal(err)
	}
	if st := enrol(es); st != wire.EnrollTokenRejected {
		t.Errorf("a token of a disabled user: %d", st)
	}
	// and a disabled user's token that was not deleted is refused too
	u4 := mk("u4")
	_, es, _ = e.store.CreateToken(e.ctx, u4, "", time.Hour)
	e.pool.Exec(e.ctx, "UPDATE app_user SET disabled = true WHERE id = $1", u4.ID)
	if st := enrol(es); st != wire.EnrollTokenRejected {
		t.Errorf("a token of a user disabled behind our back: %d", st)
	}

	// the creator may delete a token after losing the right, and the number of open tokens is capped
	u5 := mk("u5")
	for range cell.MaxOpenTokens {
		if _, _, err := e.store.CreateToken(e.ctx, u5, "", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := e.store.CreateToken(e.ctx, u5, "", time.Hour); !errors.Is(err, cell.ErrTooManyTokens) {
		t.Errorf("token number %d: %v", cell.MaxOpenTokens+1, err)
	}
	toks, _ := e.store.ListTokens(e.ctx, u5)
	e.pool.Exec(e.ctx, "UPDATE app_user SET may_add_cells = false WHERE id = $1", u5.ID)
	if err := e.store.DeleteToken(e.ctx, u5, toks[0].ID); err != nil {
		t.Errorf("a creator deleting their own token: %v", err)
	}
}

// A cell that asks for certificates in a loop is refused after a few (review finding C9).
func TestRenewalIsThrottled(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, true)
	cn, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	var refused error
	for range cell.MaxRenewalsPerHour + 2 {
		if refused = c.Renew(cn); refused != nil {
			break
		}
	}
	if refused == nil {
		t.Error("a cell renewed its certificate without limit")
	}
}

// A base station is active at once, so how many one operator may have is limited; and a disabled user's are switched off
// (review findings C4 and C5).
func TestOperatorLimitsAndDisabledOperator(t *testing.T) {
	e := newEnv(t)
	u, _ := e.users.CreateLocalUser(e.ctx, auth.NewLocalUser{Username: "op", Password: "user password", Role: auth.RoleUser, MayAddCells: true})
	for i := range cell.MaxCellsPerOperator {
		if _, err := e.pool.Exec(e.ctx, "INSERT INTO cell (name, state, enrolled_by) VALUES ($1, 'active', $2)", fmt.Sprintf("c%d", i), u.ID); err != nil {
			t.Fatal(err)
		}
	}
	_, es, _ := e.store.CreateToken(e.ctx, u, "one too many", time.Hour)
	if _, status, _ := celltest.Enroll(e.ctx, es, ""); status != wire.EnrollTokenRejected {
		t.Errorf("the %dth cell of one operator: %d", cell.MaxCellsPerOperator+1, status)
	}
	// an administrator is not limited
	_, es, _ = e.store.CreateToken(e.ctx, e.admin, "admin's", time.Hour)
	if _, status, _ := celltest.Enroll(e.ctx, es, ""); status != wire.EnrollOK {
		t.Errorf("an administrator's cell: %d", status)
	}

	ids, err := e.store.DisableCellsOf(e.ctx, u.ID)
	if err != nil || len(ids) != cell.MaxCellsPerOperator {
		t.Fatalf("disable: %d cells, %v", len(ids), err)
	}
	var active int
	e.pool.QueryRow(e.ctx, "SELECT count(*) FROM cell WHERE enrolled_by = $1 AND state = 'active'", u.ID).Scan(&active)
	if active != 0 {
		t.Errorf("%d of a disabled user's cells are still active", active)
	}
	var adminCells int
	e.pool.QueryRow(e.ctx, "SELECT count(*) FROM cell WHERE enrolled_by = $1 AND state = 'active'", e.admin.ID).Scan(&adminCells)
	if adminCells != 1 {
		t.Errorf("another operator's cell was touched: %d", adminCells)
	}
}

// failingListener fails its first n Accept calls the way a process that is out of file descriptors does.
type failingListener struct {
	net.Listener
	n atomic.Int32
}

func (l *failingListener) Accept() (net.Conn, error) {
	if l.n.Add(-1) >= 0 {
		return nil, errors.New("accept tcp: too many open files")
	}
	return l.Listener.Accept()
}

// A failing Accept is waited out: the server goes on serving, and the cells connected stay connected (finding C7).
func TestServeSurvivesFailingAccepts(t *testing.T) {
	e := newEnvOn(t, func(ln net.Listener) net.Listener {
		fl := &failingListener{Listener: ln}
		fl.n.Store(3)
		return fl
	})
	c := e.enrolled(t, true)
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	cn, err := c.Connect(ctx, celltest.Options{})
	if err != nil {
		t.Fatalf("no connection after failing accepts: %v", err)
	}
	defer cn.Close()
	expectState(t, cn, wire.StateActive)
}

// A cell that sends STATS in a loop costs the database one last-seen write per interval, not one per frame (finding C8).
func TestStatsWritesAreThrottled(t *testing.T) {
	e := newEnv(t)
	c := e.enrolled(t, true)
	cn, err := c.Connect(e.ctx, celltest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cn.Close()
	expectState(t, cn, wire.StateActive)
	lastSeen := func() time.Time {
		var at *time.Time
		if err := e.pool.QueryRow(e.ctx, "SELECT last_seen_at FROM cell WHERE id = $1", c.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if at == nil {
			return time.Time{}
		}
		return *at
	}
	// frames of one session are handled in order: an echoed heartbeat says every frame before it is done
	barrier := func(v uint32) {
		t.Helper()
		cn.Send(wire.Frame{Type: wire.Heartbeat, Body: wire.HeartbeatBody(v)})
		if _, err := cn.Expect(wire.HeartbeatEcho, 2*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	before := lastSeen()
	cn.Send(wire.Frame{Type: wire.Stats, Body: wire.EncodeStats(map[uint16]uint64{1: 1})})
	barrier(1)
	first := lastSeen()
	if !first.After(before) {
		t.Fatalf("the first STATS did not stamp the cell: %v then %v", before, first)
	}
	time.Sleep(20 * time.Millisecond) // the clock must be able to move
	for i := 2; i < 40; i++ {
		cn.Send(wire.Frame{Type: wire.Stats, Body: wire.EncodeStats(map[uint16]uint64{1: uint64(i)})})
	}
	barrier(2)
	if s := e.srv.CellStatus(c.ID); s.Stats["mails_up"] != 39 {
		t.Fatalf("the STATS frames were not handled: %v", s.Stats)
	}
	if again := lastSeen(); !again.Equal(first) {
		t.Errorf("38 more STATS frames wrote the database again: %v then %v", first, again)
	}
}
