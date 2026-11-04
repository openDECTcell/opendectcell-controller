// SPDX-License-Identifier: AGPL-3.0-or-later

package carddav_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/carddav"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

var ctx = context.Background()

// fakeServer answers Fetch with what a test says.
type fakeServer struct {
	res  carddav.Result
	err  error
	asks []string
}

func (f *fakeServer) Fetch(_ context.Context, url, user, password string) (carddav.Result, error) {
	f.asks = append(f.asks, url+"|"+user+"|"+password)
	return f.res, f.err
}

func card(href, etag, last, first, number string) carddav.Card {
	return carddav.Card{Href: href, ETag: etag, Contact: contacts.Contact{LastName: last, FirstName: first, Numbers: []contacts.Number{{Number: number, Kind: "mobile", Default: true}}}}
}

type rig struct {
	store    *carddav.Store
	contacts *contacts.Store
	srv      *fakeServer
	anna     *auth.User
	ben      *auth.User
	changed  []uuid.UUID
	hs       uuid.UUID // Anna's handset
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pool := dbtest.Migrated(t)
	ks, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(ks)
	kr, err := db.LoadKeyring(ctx, pool, mk, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{srv: &fakeServer{}, contacts: &contacts.Store{Pool: pool}}
	r.store = &carddav.Store{Pool: pool, Keyring: kr, Client: r.srv, OnChange: func(o uuid.UUID) { r.changed = append(r.changed, o) }}
	users := &auth.Store{Pool: pool}
	mkUser := func(name string) *auth.User {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, "INSERT INTO app_user (username, display_name, role, auth_source) VALUES ($1, $1, 'user', 'local') RETURNING id", name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		u, err := users.UserByID(ctx, pool, id)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	r.anna, r.ben = mkUser("anna"), mkUser("ben")
	if err := pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\\x01', 1, '\\x') RETURNING id", r.anna.ID).Scan(&r.hs); err != nil {
		t.Fatal(err)
	}
	return r
}

func good() carddav.Input {
	return carddav.Input{Label: "Cloud", URL: "https://cloud.example.org/dav/anna/", Username: "anna", Password: "secret-pw", Enabled: true, IntervalMin: 30}
}

func TestCreateValidatesAndSeals(t *testing.T) {
	r := newRig(t)
	for name, mod := range map[string]func(*carddav.Input){
		"no label": func(i *carddav.Input) { i.Label = " " },
		"http":     func(i *carddav.Input) { i.URL = "http://cloud.example.org/dav/" },
		"too fast": func(i *carddav.Input) { i.IntervalMin = 1 },
		"too slow": func(i *carddav.Input) { i.IntervalMin = 5000 },
	} {
		in := good()
		mod(&in)
		var v carddav.ValidationError
		if _, err := r.store.Create(ctx, r.anna, in, nil); !errors.As(err, &v) {
			t.Errorf("%s: %v", name, err)
		}
	}
	b, err := r.store.Create(ctx, r.anna, good(), []uuid.UUID{r.hs})
	if err != nil || !b.HasPassword || b.Label != "Cloud" {
		t.Fatalf("create: %+v %v", b, err)
	}
	var enc []byte
	r.store.Pool.QueryRow(ctx, "SELECT password_enc FROM addressbook WHERE id = $1", b.ID).Scan(&enc)
	if len(enc) == 0 || strings.Contains(string(enc), "secret-pw") {
		t.Errorf("the password is not sealed: %q", enc)
	}
	if hb, _ := r.store.HandsetBooks(ctx, r.hs); len(hb) != 1 || hb[0] != b.ID {
		t.Errorf("the book was not attached to the handset: %v", hb)
	}
	var v carddav.ValidationError
	if _, err := r.store.Create(ctx, r.anna, good(), nil); !errors.As(err, &v) || !strings.Contains(v["label"], "with that name") {
		t.Errorf("the same name twice: %v", err)
	}
	// somebody else's handset is not attached
	in := good()
	in.Label = "Other"
	r.store.Create(ctx, r.ben, in, []uuid.UUID{r.hs})
	if hb, _ := r.store.HandsetBooks(ctx, r.hs); len(hb) != 1 {
		t.Errorf("another user attached a book to Anna's handset: %v", hb)
	}
	// the limit
	for i := 0; i < carddav.MaxBooksPerUser; i++ {
		in := good()
		in.Label = fmt.Sprintf("B%d", i)
		if _, err := r.store.Create(ctx, r.anna, in, nil); err != nil {
			break
		}
	}
	in = good()
	in.Label = "One too many"
	if _, err := r.store.Create(ctx, r.anna, in, nil); !errors.As(err, &v) || !strings.Contains(v["label"], "at most") {
		t.Errorf("the limit: %v", err)
	}
}

func TestSyncKeepsTheContactsInStep(t *testing.T) {
	r := newRig(t)
	b, _ := r.store.Create(ctx, r.anna, good(), []uuid.UUID{r.hs})
	r.srv.res = carddav.Result{Total: 2, Cards: []carddav.Card{card("/a", "e1", "Schmidt", "Anna", "+49170111"), card("/b", "e1", "Meier", "Ben", "030222")}}
	if err := r.store.Sync(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if r.srv.asks[0] != "https://cloud.example.org/dav/anna/|anna|secret-pw" {
		t.Errorf("asked %q", r.srv.asks[0])
	}
	got, _ := r.store.Get(ctx, r.anna, b.ID)
	if got.Count != 2 || got.LastOKAt == nil || got.LastError != "" {
		t.Fatalf("book after sync: %+v", got)
	}
	if len(r.changed) != 1 || r.changed[0] != r.anna.ID {
		t.Errorf("the phonebooks were not refreshed: %v", r.changed)
	}
	names := func() string {
		cs, err := r.contacts.ForHandset(ctx, r.hs)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range cs {
			out = append(out, c.LastName+":"+c.Numbers[0].Number)
		}
		return strings.Join(out, ",")
	}
	if got := names(); got != "Meier:030222,Schmidt:+49170111" {
		t.Errorf("handset reads %q", got)
	}

	// unchanged etags: nothing is written and nobody is told
	r.changed = nil
	if err := r.store.Sync(ctx, b.ID); err != nil || len(r.changed) != 0 {
		t.Fatalf("a sync with nothing new: %v, told %v", err, r.changed)
	}
	// one card changed, one gone, one new
	r.srv.res = carddav.Result{Total: 2, Cards: []carddav.Card{card("/a", "e2", "Schmidt", "Anna", "+49170999"), card("/c", "e1", "Neu", "Nina", "0404040")}}
	if err := r.store.Sync(ctx, b.ID); err != nil || len(r.changed) != 1 {
		t.Fatalf("sync: %v %v", err, r.changed)
	}
	if got := names(); got != "Neu:0404040,Schmidt:+49170999" {
		t.Errorf("after the changes the handset reads %q", got)
	}
	// the server cannot be reached: the contacts stay, the book says why
	r.srv.err = errors.New("the server refused the user name or password")
	if err := r.store.Sync(ctx, b.ID); err == nil {
		t.Fatal("a failed sync reports nothing")
	}
	got, _ = r.store.Get(ctx, r.anna, b.ID)
	if got.LastError != "the server refused the user name or password" || got.Count != 2 {
		t.Errorf("book after a failure: %+v", got)
	}
	if names() != "Neu:0404040,Schmidt:+49170999" {
		t.Errorf("a failed sync took the contacts away: %q", names())
	}
	// what could not be used is said, and is not an error
	r.srv.err = nil
	r.srv.res = carddav.Result{Total: 1, Cards: []carddav.Card{card("/a", "e3", "Schmidt", "Anna", "1")}, Skipped: []string{"card 2: no number"}}
	r.store.Sync(ctx, b.ID)
	got, _ = r.store.Get(ctx, r.anna, b.ID)
	if got.LastError != "" || !strings.Contains(got.LastNote, "1 could not be used: card 2: no number") {
		t.Errorf("note: %+v", got)
	}
}

func TestBooksAreShownOnTheHandsetsTheyAreTickedFor(t *testing.T) {
	r := newRig(t)
	var other uuid.UUID
	r.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\\x02', 2, '\\x') RETURNING id", r.anna.ID).Scan(&other)
	b, _ := r.store.Create(ctx, r.anna, good(), []uuid.UUID{r.hs}) // only the first handset
	r.srv.res = carddav.Result{Total: 1, Cards: []carddav.Card{card("/a", "e1", "Schmidt", "Anna", "+49170111")}}
	r.store.Sync(ctx, b.ID)
	r.contacts.Create(ctx, r.anna, contacts.Contact{LastName: "Mine", Numbers: []contacts.Number{{Number: "1", Kind: "other"}}}, false)
	count := func(h uuid.UUID) int { cs, _ := r.contacts.ForHandset(ctx, h); return len(cs) }
	if count(r.hs) != 2 || count(other) != 1 {
		t.Errorf("handsets read %d and %d contacts, want 2 and 1", count(r.hs), count(other))
	}
	// ticking it for the other one, the way the handset page does
	if err := pgx.BeginFunc(ctx, r.store.Pool, func(tx pgx.Tx) error {
		return carddav.SetHandsetBooks(ctx, tx, r.anna.ID, other, []uuid.UUID{b.ID})
	}); err != nil {
		t.Fatal(err)
	}
	if count(other) != 2 {
		t.Errorf("the second handset reads %d", count(other))
	}
	// somebody else's book cannot be ticked
	bb, _ := r.store.Create(ctx, r.ben, good(), nil)
	pgx.BeginFunc(ctx, r.store.Pool, func(tx pgx.Tx) error {
		return carddav.SetHandsetBooks(ctx, tx, r.anna.ID, other, []uuid.UUID{b.ID, bb.ID})
	})
	if hb, _ := r.store.HandsetBooks(ctx, other); len(hb) != 1 || hb[0] != b.ID {
		t.Errorf("a stranger's book is on the handset: %v", hb)
	}
	// the user's own list and the API do not hold the book's contacts, and they cannot be changed
	mine, _ := r.contacts.List(ctx, r.anna, "", "")
	if len(mine) != 1 || mine[0].LastName != "Mine" {
		t.Errorf("the user's list holds %d contacts", len(mine))
	}
	var bookContact int64
	r.store.Pool.QueryRow(ctx, "SELECT id FROM contact WHERE addressbook_id = $1", b.ID).Scan(&bookContact)
	if _, err := r.contacts.Update(ctx, r.anna, bookContact, contacts.Contact{LastName: "X", Numbers: []contacts.Number{{Number: "5", Kind: "other"}}}); !errors.Is(err, contacts.ErrReadOnly) {
		t.Errorf("update of a book's contact: %v", err)
	}
	if err := r.contacts.Delete(ctx, r.anna, bookContact); !errors.Is(err, contacts.ErrReadOnly) {
		t.Errorf("delete of a book's contact: %v", err)
	}
	if _, err := r.contacts.SaveFromHandset(ctx, r.hs, bookContact, contacts.Contact{LastName: "X"}); !errors.Is(err, contacts.ErrReadOnly) {
		t.Errorf("a handset editing a book's contact: %v", err)
	}
	// unlinking the book takes its contacts along, and tells the phonebooks
	r.changed = nil
	if err := r.store.Delete(ctx, r.anna, b.ID); err != nil || len(r.changed) != 1 {
		t.Fatalf("delete: %v %v", err, r.changed)
	}
	if count(r.hs) != 1 {
		t.Errorf("the handset still reads %d contacts", count(r.hs))
	}
	// and another user cannot delete or read it
	if err := r.store.Delete(ctx, r.anna, bb.ID); !errors.Is(err, carddav.ErrNotFound) {
		t.Errorf("deleting Ben's book as Anna: %v", err)
	}
	if _, err := r.store.Get(ctx, r.anna, bb.ID); !errors.Is(err, carddav.ErrNotFound) {
		t.Errorf("reading Ben's book as Anna: %v", err)
	}
}

func TestADeadBookDoesNotCountAgainstTheManualLimit(t *testing.T) {
	r := newRig(t)
	b, _ := r.store.Create(ctx, r.anna, good(), nil)
	var cards []carddav.Card
	for i := 0; i < 500; i++ {
		cards = append(cards, card(fmt.Sprintf("/%d", i), "e", fmt.Sprintf("L%d", i), "F", fmt.Sprintf("1%04d", i)))
	}
	r.srv.res = carddav.Result{Total: 500, Cards: cards}
	if err := r.store.Sync(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := r.contacts.Create(ctx, r.anna, contacts.Contact{LastName: fmt.Sprintf("M%d", i), Numbers: []contacts.Number{{Number: "1", Kind: "other"}}}, false); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	r.store.Pool.QueryRow(ctx, "SELECT count(*) FROM contact WHERE owner_user_id = $1", r.anna.ID).Scan(&n)
	if n != 505 {
		t.Errorf("%d contacts", n)
	}
}

func TestDue(t *testing.T) {
	r := newRig(t)
	now := time.Now()
	r.store.Now = func() time.Time { return now }
	b, _ := r.store.Create(ctx, r.anna, good(), nil)
	due := func(at time.Time) int { ids, _ := r.store.Due(ctx, at); return len(ids) }
	if due(now) != 1 {
		t.Error("a book never synced is not due")
	}
	r.srv.res = carddav.Result{}
	r.store.Sync(ctx, b.ID)
	if due(now.Add(10*time.Minute)) != 0 || due(now.Add(31*time.Minute)) != 1 {
		t.Errorf("a book with 30 minutes is due at %d / %d", due(now.Add(10*time.Minute)), due(now.Add(31*time.Minute)))
	}
	r.store.SetEnabled(ctx, r.anna, b.ID, false)
	if due(now.Add(time.Hour)) != 0 {
		t.Error("a switched-off book is due")
	}
}

func TestUpdate(t *testing.T) {
	r := newRig(t)
	b, _ := r.store.Create(ctx, r.anna, good(), nil)
	in := good()
	in.Label, in.URL, in.Password, in.IntervalMin = "Renamed", "https://other.example.org/dav/", "", 120
	got, err := r.store.Update(ctx, r.anna, b.ID, in)
	if err != nil || got.Label != "Renamed" || got.URL != "https://other.example.org/dav/" || got.IntervalMin != 120 || !got.HasPassword {
		t.Fatalf("update: %+v %v", got, err)
	}
	in.Password = "new-secret"
	r.store.Update(ctx, r.anna, b.ID, in)
	r.srv.res = carddav.Result{}
	r.store.Sync(ctx, b.ID)
	if last := r.srv.asks[len(r.srv.asks)-1]; last != "https://other.example.org/dav/|anna|new-secret" {
		t.Errorf("the next sync asked %q", last)
	}
	// the name of another of the user's books is taken; somebody else's book is not theirs to change
	other := good()
	other.Label = "Second"
	r.store.Create(ctx, r.anna, other, nil)
	in.Label = "Second"
	var v carddav.ValidationError
	if _, err := r.store.Update(ctx, r.anna, b.ID, in); !errors.As(err, &v) {
		t.Errorf("a name already used: %v", err)
	}
	if _, err := r.store.Update(ctx, r.ben, b.ID, good()); !errors.Is(err, carddav.ErrNotFound) {
		t.Errorf("Ben changing Anna's book: %v", err)
	}
}
