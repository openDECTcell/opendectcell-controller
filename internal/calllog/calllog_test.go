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
