// SPDX-License-Identifier: AGPL-3.0-or-later

package calllog_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/media"
)

var ctx = context.Background()

func TestCallLifecycle(t *testing.T) {
	pool := dbtest.Migrated(t)
	s := &calllog.Store{Pool: pool}
	var owner, handset uuid.UUID
	if err := pool.QueryRow(ctx, "INSERT INTO app_user (username, display_name, role, auth_source) VALUES ('anna', 'Anna', 'user', 'local') RETURNING id").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\\x01', 1, '\\x') RETURNING id", owner).Scan(&handset); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	id, err := s.Start(ctx, calllog.Call{HandsetID: &handset, Direction: "out", RemoteParty: "210", StartedAt: start})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Answered(ctx, id, start.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	rtt := 42.0
	q := &media.Quality{SIP: media.LegQuality{Packets: 1000, LossPct: 0.5, JitterMs: 3, RTTMs: &rtt}, Cell: media.LegQuality{Packets: 1000}, MOS: 4.3, Plan: "SIP PCMA"}
	yes := true
	if err := s.Ended(ctx, id, calllog.End{At: start.Add(65 * time.Second), Reason: "normal", SIPCodec: "PCMA", CellCodec: "L16", SRTP: &yes, Quality: q}); err != nil {
		t.Fatal(err)
	}
	// A missed call with no media.
	id2, err := s.Start(ctx, calllog.Call{HandsetID: &handset, Direction: "in", RemoteParty: "+4930123", StartedAt: start.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ended(ctx, id2, calllog.End{At: start.Add(time.Hour + 20*time.Second), Reason: "no_answer"}); err != nil {
		t.Fatal(err)
	}

	calls, err := s.List(ctx, calllog.Filter{})
	if err != nil || len(calls) != 2 {
		t.Fatalf("%d calls, %v", len(calls), err)
	}
	missed, done := calls[0], calls[1]
	if missed.ID != id2 || missed.Quality != nil || missed.Duration() != 0 || *missed.EndReason != "no_answer" {
		t.Errorf("missed call %+v", missed)
	}
	if done.Duration() != time.Minute || *done.OwnerID != owner || *done.SIPCodec != "PCMA" || !*done.SRTP {
		t.Errorf("call %+v", done)
	}
	if done.Quality == nil || done.Quality.MOS != 4.3 || *done.Quality.SIP.RTTMs != 42 || done.Quality.Plan != "SIP PCMA" {
		t.Errorf("quality %+v", done.Quality)
	}

	other := uuid.New()
	if calls, _ := s.List(ctx, calllog.Filter{Owner: &other}); len(calls) != 0 {
		t.Error("owner filter leaks")
	}
	if calls, _ := s.List(ctx, calllog.Filter{Before: id2}); len(calls) != 1 || calls[0].ID != id {
		t.Error("cursor")
	}
	if err := s.Answered(ctx, 999999, start); !errors.Is(err, calllog.ErrNotFound) {
		t.Errorf("unknown call: %v", err)
	}
}

// The filters of the user's Calls page: direction, missed, line, handset, owner; one call by id; the read marks.
func TestFiltersAndReadMarks(t *testing.T) {
	pool := dbtest.Migrated(t)
	s := &calllog.Store{Pool: pool}
	var anna, ben, hs, acct uuid.UUID
	pool.QueryRow(ctx, "INSERT INTO app_user (username, display_name, role, auth_source) VALUES ('anna', 'Anna', 'user', 'local') RETURNING id").Scan(&anna)
	pool.QueryRow(ctx, "INSERT INTO app_user (username, display_name, role, auth_source) VALUES ('ben', 'Ben', 'user', 'local') RETURNING id").Scan(&ben)
	pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\\x01', 1, '\\x') RETURNING id", anna).Scan(&hs)
	pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Home', '1', 'sip.example.org') RETURNING id", anna).Scan(&acct)
	t0 := time.Now().Add(-48 * time.Hour)
	mk := func(owner uuid.UUID, dir string, answered bool, at time.Duration, handset, account *uuid.UUID) int64 {
		id, err := s.Start(ctx, calllog.Call{OwnerID: &owner, HandsetID: handset, SIPAccountID: account, Direction: dir, RemoteParty: "+49301", StartedAt: t0.Add(at)})
		if err != nil {
			t.Fatal(err)
		}
		if answered {
			s.Answered(ctx, id, t0.Add(at+time.Second))
		}
		if err := s.Ended(ctx, id, calllog.End{At: t0.Add(at + time.Minute), Reason: "normal"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	out1 := mk(anna, "out", true, 0, &hs, &acct)
	in1 := mk(anna, "in", true, time.Hour, &hs, &acct)
	miss1 := mk(anna, "in", false, 2*time.Hour, nil, &acct)
	miss2 := mk(anna, "in", false, 3*time.Hour, nil, nil)
	_ = mk(ben, "in", false, 4*time.Hour, nil, nil)
	ids := func(f calllog.Filter) []int64 {
		f.Owner = &anna
		cs, err := s.List(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	eq := func(name string, got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: %v, want %v", name, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: %v, want %v", name, got, want)
				return
			}
		}
	}
	eq("all of anna's, newest first", ids(calllog.Filter{}), miss2, miss1, in1, out1)
	eq("outgoing", ids(calllog.Filter{Direction: "out"}), out1)
	eq("incoming", ids(calllog.Filter{Direction: "in"}), miss2, miss1, in1)
	eq("missed", ids(calllog.Filter{Missed: true}), miss2, miss1)
	eq("one line", ids(calllog.Filter{Account: &acct}), miss1, in1, out1)
	eq("one handset", ids(calllog.Filter{Handset: &hs}), in1, out1)
	eq("missed on one line", ids(calllog.Filter{Missed: true, Account: &acct}), miss1)
	eq("since", ids(calllog.Filter{Since: t0.Add(90 * time.Minute)}), miss2, miss1)

	c, err := s.Get(ctx, miss1)
	if err != nil || !c.Missed() || c.SIPAccountID == nil {
		t.Errorf("Get: %+v %v", c, err)
	}
	if _, err := s.Get(ctx, 1<<40); !errors.Is(err, calllog.ErrNotFound) {
		t.Errorf("Get of nothing: %v", err)
	}
	if n, _ := s.UnreadMissed(ctx, anna); n != 2 {
		t.Errorf("%d unread missed calls, want 2", n)
	}
	if n, err := s.MarkMissedRead(ctx, anna); err != nil || n != 2 {
		t.Errorf("marked %d, %v", n, err)
	}
	if n, _ := s.UnreadMissed(ctx, anna); n != 0 {
		t.Errorf("%d unread after marking", n)
	}
	if n, _ := s.UnreadMissed(ctx, ben); n != 1 {
		t.Errorf("ben's unread missed call was marked by anna: %d", n)
	}
}

// The hook is told after the call is in the log (so it can read the row), once for the start and once for the end, and not when nothing
// was recorded.
func TestHookIsToldAfterTheRowIsWritten(t *testing.T) {
	pool := dbtest.Migrated(t)
	type seen struct {
		event  string
		id     int64
		exists bool
		ended  bool
	}
	var got []seen
	s := &calllog.Store{Pool: pool}
	s.Hook = func(hctx context.Context, event string, id int64) {
		c, err := s.Get(hctx, id)
		got = append(got, seen{event, id, err == nil, err == nil && c.EndedAt != nil})
	}
	id, err := s.Start(ctx, calllog.Call{Direction: "in", RemoteParty: "+4930123", StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Answered(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Ended(ctx, id, calllog.End{At: time.Now(), Reason: "normal"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (seen{calllog.EventStarted, id, true, false}) || got[1] != (seen{calllog.EventEnded, id, true, true}) {
		t.Fatalf("hook calls: %+v", got)
	}
	// ending a call that is not there records nothing, and tells nobody
	got = nil
	if err := s.Ended(ctx, 987654, calllog.End{At: time.Now()}); err == nil {
		t.Fatal("ending an unknown call is not an error")
	}
	if len(got) != 0 {
		t.Errorf("the hook was told of a call that was not recorded: %+v", got)
	}
	// without a hook nothing breaks
	if _, err := (&calllog.Store{Pool: pool}).Start(ctx, calllog.Call{Direction: "out", StartedAt: time.Now()}); err != nil {
		t.Error(err)
	}
}
