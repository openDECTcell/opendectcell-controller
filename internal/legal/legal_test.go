// SPDX-License-Identifier: AGPL-3.0-or-later

package legal_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/legal"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
)

var ctx = context.Background()

func newStore(t *testing.T) (*legal.Store, uuid.UUID) {
	t.Helper()
	pool := dbtest.Migrated(t)
	s := &legal.Store{Pool: pool, Settings: &settings.Store{Pool: pool}}
	var u uuid.UUID
	if err := pool.QueryRow(ctx, "INSERT INTO app_user (username, role, auth_source) VALUES ('anna', 'user', 'local') RETURNING id").Scan(&u); err != nil {
		t.Fatal(err)
	}
	return s, u
}

func TestNothingEnabledNothingPending(t *testing.T) {
	s, u := newStore(t)
	if p, err := s.Pending(ctx, u); err != nil || len(p) != 0 {
		t.Fatalf("pending %v %v", p, err)
	}
	if e, _ := s.Enabled(ctx); len(e) != 0 {
		t.Errorf("enabled %v", e)
	}
}

func TestTextsThatNeedWordsCannotBeSwitchedOnEmpty(t *testing.T) {
	s, _ := newStore(t)
	for _, k := range []legal.Kind{legal.Imprint, legal.Privacy} {
		err := s.Save(ctx, map[legal.Kind]legal.Input{k: {Enabled: true, Text: "  "}})
		if !errors.Is(err, legal.ErrInvalid) {
			t.Errorf("%s with no text: %v", k, err)
		}
	}
	// the other two have a standard text and may be switched on empty
	if err := s.Save(ctx, map[legal.Kind]legal.Input{legal.Emergency: {Enabled: true}, legal.Experimental: {Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Enabled(ctx)
	if len(e) != 2 || e[0].Kind != legal.Emergency || e[1].Kind != legal.Experimental {
		t.Fatalf("enabled %+v", e)
	}
	if !strings.Contains(strings.Join(e[0].Paragraphs, " "), "112") || !strings.Contains(strings.Join(e[1].Paragraphs, " "), "no guaranteed service") {
		t.Errorf("the standard texts are not shown: %+v", e)
	}
	if err := s.Save(ctx, map[legal.Kind]legal.Input{legal.Imprint: {Enabled: true, Text: strings.Repeat("x", legal.MaxText+1)}}); !errors.Is(err, legal.ErrInvalid) {
		t.Errorf("a text that is too long: %v", err)
	}
}

func TestSignOffAndTheVersionThatMakesEveryoneSignAgain(t *testing.T) {
	s, u := newStore(t)
	in := map[legal.Kind]legal.Input{
		legal.Imprint:      {Enabled: true, Text: "Example e.V., Examplestreet 1"},
		legal.Experimental: {Enabled: true, Text: "Expect downtime on Mondays."},
	}
	if err := s.Save(ctx, in); err != nil {
		t.Fatal(err)
	}
	p, err := s.Pending(ctx, u)
	if err != nil || len(p) != 2 {
		t.Fatalf("pending %+v %v", p, err)
	}
	// ticking only one is refused and records nothing
	if err := s.Accept(ctx, u, map[legal.Kind]int{legal.Imprint: p[0].Version}); !errors.Is(err, legal.ErrNotAll) {
		t.Fatalf("one box: %v", err)
	}
	if p, _ := s.Pending(ctx, u); len(p) != 2 {
		t.Fatalf("a refused sign-off recorded something: %+v", p)
	}
	// a version the user was not shown is refused too
	if err := s.Accept(ctx, u, map[legal.Kind]int{legal.Imprint: 99, legal.Experimental: 99}); !errors.Is(err, legal.ErrNotAll) {
		t.Fatalf("wrong version: %v", err)
	}
	if err := s.Accept(ctx, u, map[legal.Kind]int{legal.Imprint: p[0].Version, legal.Experimental: p[1].Version}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Pending(ctx, u); len(p) != 0 {
		t.Fatalf("after the sign-off: %+v", p)
	}

	// saving the same words again keeps the versions: nobody is asked twice for nothing
	if err := s.Save(ctx, in); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Pending(ctx, u); len(p) != 0 {
		t.Fatalf("an unchanged save asked again: %+v", p)
	}
	// changing one text asks for that one only
	in[legal.Imprint] = legal.Input{Enabled: true, Text: "Example e.V., Newstreet 2"}
	if err := s.Save(ctx, in); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Pending(ctx, u)
	if len(p) != 1 || p[0].Kind != legal.Imprint || p[0].Version != 2 {
		t.Fatalf("after a change: %+v", p)
	}
	// a text switched off is no longer asked for, and asked for again when it is switched on
	delete(in, legal.Imprint)
	if err := s.Save(ctx, in); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Pending(ctx, u); len(p) != 0 {
		t.Fatalf("a text that is off is pending: %+v", p)
	}
	in[legal.Imprint] = legal.Input{Enabled: true, Text: "Example e.V., Newstreet 2"}
	if err := s.Save(ctx, in); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Pending(ctx, u); len(p) != 1 || p[0].Version != 3 {
		t.Fatalf("switched on again: %+v", p)
	}
}

func TestEraseRemovesTheSignOffs(t *testing.T) {
	s, u := newStore(t)
	if err := s.Save(ctx, map[legal.Kind]legal.Input{legal.Emergency: {Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Pending(ctx, u)
	if err := s.Accept(ctx, u, map[legal.Kind]int{legal.Emergency: p[0].Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "DELETE FROM app_user WHERE id = $1", u); err != nil {
		t.Fatal(err)
	}
	var n int
	s.Pool.QueryRow(ctx, "SELECT count(*) FROM legal_acceptance").Scan(&n)
	if n != 0 {
		t.Errorf("%d sign-offs left after the user went", n)
	}
}
