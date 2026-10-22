// SPDX-License-Identifier: AGPL-3.0-or-later

package cell_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

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
	store *cell.Store
	srv   *cell.Server
	admin *auth.User
	users *auth.Store
	ctx   context.Context
}

func newEnv(t *testing.T) *env {
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
		Orphan: 2 * time.Second, EnrollDelay: 10 * time.Millisecond}
	done := make(chan struct{})
	go func() { srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	users := &auth.Store{Pool: pool, Argon: auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1}, SessionIdle: time.Hour, SessionMaxAge: time.Hour, RateLimit: 5}
	admin, err := users.CreateLocalUser(ctx, auth.NewLocalUser{Username: "admin", Password: "admin password", Role: auth.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	return &env{store: st, srv: srv, admin: admin, users: users, ctx: audit.WithActor(ctx, admin.Actor())}
}

func (e *env) enrolled(t *testing.T, auto bool) *celltest.Cell {
	t.Helper()
	_, es, err := e.store.CreateToken(e.ctx, e.admin, "test", 0, auto)
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
	_, es, err := e.store.CreateToken(e.ctx, e.admin, "Garage", time.Hour, false)
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
	_, es2, _ := e.store.CreateToken(e.ctx, e.admin, "Garage", time.Hour, true)
	c2, _, _ := celltest.Enroll(e.ctx, es2, "")
	got2, _ := e.store.GetCell(e.ctx, e.admin, c2.ID)
	if c2.State != wire.StateActive || got2.Name != "Garage-2" {
		t.Errorf("auto-approved: %s %q", c2.State, got2.Name)
	}

	// Rate limit: 5 rejected tokens from one address, then RATE_LIMITED.
	// The reused token above was the first.
	for i := 0; i < 4; i++ {
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
	if _, _, err := e.store.CreateToken(e.ctx, u, "", 0, false); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("user without may_add_cells: %v", err)
	}
	u.MayAddCells = true
	if _, _, err := e.store.CreateToken(e.ctx, u, "", 0, true); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("user set auto-approve: %v", err)
	}
	mine, _, err := e.store.CreateToken(e.ctx, u, "mine", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	adm, _, _ := e.store.CreateToken(e.ctx, e.admin, "admin's", 0, false)
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
	if _, _, err := e.store.CreateToken(e.ctx, e.admin, "", time.Minute, false); err == nil {
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
	// Mails from an active cell are accepted (and dropped until C3).
	cn.Send(wire.Frame{Type: wire.MailUp, Body: make([]byte, 9)})
	time.Sleep(100 * time.Millisecond)
	if e.srv.CellStatus(c.ID).MailsDrop != 1 {
		t.Error("mail from an active cell not counted")
	}
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
