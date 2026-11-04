// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/carddav"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
)

type fakeBook struct {
	res carddav.Result
	err error
}

func (f *fakeBook) Fetch(context.Context, string, string, string) (carddav.Result, error) {
	return f.res, f.err
}

func bookEnv(t *testing.T) (*env, *fakeBook, *carddav.Store) {
	fb := &fakeBook{res: carddav.Result{Total: 2, Cards: []carddav.Card{
		{Href: "/a", ETag: "1", Contact: contacts.Contact{LastName: "Schmidt", FirstName: "Anna", Numbers: []contacts.Number{{Number: "+49170111", Kind: "mobile"}}}},
		{Href: "/b", ETag: "1", Contact: contacts.Contact{LastName: "Meier", Numbers: []contacts.Number{{Number: "030222", Kind: "work"}}}},
	}}}
	var books *carddav.Store
	e := newEnvWith(t, func(s *Server) {
		kr, err := db.LoadKeyring(ctx, s.Pool, testMK, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.Contacts = &contacts.Store{Pool: s.Pool}
		books = &carddav.Store{Pool: s.Pool, Keyring: kr, Client: fb}
		s.Books = books
	})
	return e, fb, books
}

func bookForm(label, u string) url.Values {
	return url.Values{"label": {label}, "url": {u}, "username": {"anna"}, "password": {"pw-secret"}, "all_handsets": {"on"}}
}

func TestAddressBookPages(t *testing.T) {
	e, fb, books := bookEnv(t)
	var uid, hs uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x0f', 15, '\\x', 'Hall') RETURNING id", uid).Scan(&hs)
	u := e.userBrowser(t)

	// a wrong address is refused with the reason, and nothing is linked
	r := e.post(t, u, "/address-books", bookForm("Cloud", "http://cloud.example.org/"))
	if body := readBody(r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "must start with https://") || strings.Contains(body, "pw-secret") {
		t.Fatalf("http address: %d %s", r.StatusCode, body)
	}
	// a good one is linked and read at once, and shown on the handsets
	r = e.post(t, u, "/address-books", bookForm("Cloud", "https://cloud.example.org/dav/"))
	if r.StatusCode != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "Synchronised%3A+2+contacts") {
		t.Fatalf("link: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	_, page := e.get(t, u, "/contacts")
	for _, want := range []string{">Cloud<", "https://cloud.example.org/dav/", "2 contacts"} {
		if !strings.Contains(page, want) {
			t.Errorf("the contacts page lacks %q", want)
		}
	}
	if strings.Contains(page, "Schmidt") || strings.Contains(page, "pw-secret") {
		t.Errorf("the book's contacts are listed as the user's own, or the password shows")
	}
	var bid uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM addressbook").Scan(&bid)
	if cs, _ := (&contacts.Store{Pool: e.store.Pool}).ForHandset(ctx, hs); len(cs) != 2 {
		t.Errorf("the handset reads %d contacts, want the book's 2", len(cs))
	}

	// the page of the book: edit keeps the password, a failing server is said
	loc := "/address-books/" + bid.String()
	if resp, p := e.get(t, u, loc); resp.StatusCode != http.StatusOK || !strings.Contains(p, "unchanged") || strings.Contains(p, "pw-secret") {
		t.Fatalf("book page: %d", resp.StatusCode)
	}
	fb.err = errors.New("the server refused the user name or password")
	r = e.post(t, u, loc, url.Values{"label": {"Cloud"}, "url": {"https://cloud.example.org/dav/"}, "username": {"anna"}, "interval": {"30"}, "enabled": {"on"}})
	if r.StatusCode != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "could+not+be+read") {
		t.Fatalf("save: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	var pwStill bool
	e.store.Pool.QueryRow(ctx, "SELECT password_enc IS NOT NULL FROM addressbook WHERE id = $1", bid).Scan(&pwStill)
	if !pwStill {
		t.Error("saving without a password removed the stored one")
	}
	_, p := e.get(t, u, loc)
	if !strings.Contains(p, "the server refused the user name or password") || !strings.Contains(p, "2") {
		t.Errorf("the problem is not on the page: %s", p)
	}
	// an invalid edit is shown again with the reason
	r = e.post(t, u, loc, url.Values{"label": {"Cloud"}, "url": {"ftp://x"}, "interval": {"30"}})
	if body := readBody(r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "must start with https://") {
		t.Errorf("bad edit: %d", r.StatusCode)
	}

	// another user can neither open, read nor remove it
	other := e.adminBrowser(t)
	for _, c := range []struct{ method, path string }{{"GET", loc}, {"POST", loc + "/sync"}, {"POST", loc + "/delete"}, {"POST", loc}} {
		var resp *http.Response
		if c.method == "GET" {
			resp, _ = e.get(t, other, c.path)
		} else {
			resp = e.post(t, other, c.path, url.Values{"label": {"x"}, "url": {"https://x.org/"}})
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s as another user: %d", c.method, c.path, resp.StatusCode)
		}
	}
	if _, err := books.Get(ctx, mustUser(t, e, "user"), bid); err != nil {
		t.Errorf("the book is gone after strangers' requests: %v", err)
	}

	// unlinking
	if r := e.post(t, u, loc+"/delete", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if cs, _ := (&contacts.Store{Pool: e.store.Pool}).ForHandset(ctx, hs); len(cs) != 0 {
		t.Errorf("the handset still reads %d contacts", len(cs))
	}
}

// The handset page ticks the books the handset shows, and saving it without that section leaves them alone.
func TestHandsetPageChoosesAddressBooks(t *testing.T) {
	e, _, books := bookEnv(t)
	var uid, hs uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x10', 16, '\\x', 'Hall') RETURNING id", uid).Scan(&hs)
	u := e.userBrowser(t)
	me := mustUser(t, e, "user")
	b, err := books.Create(ctx, me, carddav.Input{Label: "Cloud", URL: "https://cloud.example.org/dav/", Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	books.Sync(ctx, b.ID)
	_, page := e.get(t, u, "/handsets/"+hs.String())
	if !strings.Contains(page, `name="book" value="`+b.ID.String()+`"`) || strings.Contains(page, `value="`+b.ID.String()+`" checked`) {
		t.Fatalf("the handset page does not offer the book unticked")
	}
	save := func(v url.Values) {
		t.Helper()
		v.Set("name", "Hall")
		v.Set("dual_ring", "call_waiting")
		if r := e.post(t, u, "/handsets/"+hs.String(), v); r.StatusCode != http.StatusSeeOther {
			t.Fatalf("save: %d", r.StatusCode)
		}
	}
	ticked := func() int { ids, _ := books.HandsetBooks(ctx, hs); return len(ids) }
	save(url.Values{"books_present": {"1"}, "book": {b.ID.String()}})
	if ticked() != 1 {
		t.Fatal("the book was not ticked")
	}
	_, page = e.get(t, u, "/handsets/"+hs.String())
	if !strings.Contains(page, `value="`+b.ID.String()+`" checked`) {
		t.Errorf("the page does not show it ticked")
	}
	save(url.Values{}) // a form without the section
	if ticked() != 1 {
		t.Error("a save without the section changed the books")
	}
	save(url.Values{"books_present": {"1"}}) // the section with nothing ticked
	if ticked() != 0 {
		t.Error("unticking did not work")
	}
	// somebody else's book id in the form is ignored
	other, _ := books.Create(ctx, mustUser(t, e, "admin"), carddav.Input{Label: "Theirs", URL: "https://x.example.org/", Enabled: true}, nil)
	save(url.Values{"books_present": {"1"}, "book": {other.ID.String()}})
	if ticked() != 0 {
		t.Error("another user's book was ticked")
	}
}
