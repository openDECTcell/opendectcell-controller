// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
)

func TestForbiddenKey(t *testing.T) {
	for _, k := range []string{"password", "new_password", "uak", "apiKey", "client-secret", "PIN", "tokens", "session.cookie"} {
		if !forbiddenKey(k) {
			t.Errorf("%q allowed", k)
		}
	}
	for _, k := range []string{"username", "mapping", "shipping", "role", "keepalive", "key_id", "dek_id", "token_count", "cert_fingerprint", "monkey", "keyword"} {
		if forbiddenKey(k) {
			t.Errorf("%q refused", k)
		}
	}
}

func TestRecordAndList(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	uid := uuid.New()
	a := Actor{UserID: &uid, Name: "alice", Addr: netip.MustParseAddr("::ffff:192.0.2.7")}
	ctx = WithActor(ctx, a)
	if err := Record(ctx, pool, Entry{Action: "user.role_change", ObjectType: "user", ObjectID: "u1",
		Details: map[string]any{"from": "user", "to": "admin"}}); err != nil {
		t.Fatal(err)
	}
	if err := RecordAs(ctx, pool, System, Entry{Action: "cell.offline", ObjectType: "cell", ObjectID: "c1"}); err != nil {
		t.Fatal(err)
	}
	err := Record(ctx, pool, Entry{Action: "x", Details: map[string]any{"nested": map[string]any{"password": "hunter2"}}})
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("secret in details: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatal("secret value echoed in error")
	}

	all, err := List(ctx, pool, Filter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("List: %d %v", len(all), err)
	}
	if all[0].Action != "cell.offline" || all[0].ActorID != nil || all[0].ActorName != "system" {
		t.Errorf("newest: %+v", all[0])
	}
	r := all[1]
	if r.ActorName != "alice" || *r.ActorID != uid || r.Details["to"] != "admin" || r.RemoteAddr.String() != "192.0.2.7" {
		t.Errorf("entry: %+v (addr %v)", r, r.RemoteAddr)
	}
	mine, _ := List(ctx, pool, Filter{ActorID: &uid})
	cells, _ := List(ctx, pool, Filter{ObjectType: "cell"})
	older, _ := List(ctx, pool, Filter{Before: all[0].ID})
	if len(mine) != 1 || len(cells) != 1 || len(older) != 1 || older[0].ID != r.ID {
		t.Errorf("filters: %d %d %d", len(mine), len(cells), len(older))
	}
}
