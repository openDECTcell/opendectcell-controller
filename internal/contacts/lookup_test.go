// SPDX-License-Identifier: AGPL-3.0-or-later

package contacts_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
)

func TestSameNumber(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"+49301234567", "0301234567", true},
		{"+49 30 1234567", "030/1234567", true},
		{"0049301234567", "+49301234567", true},
		{"301234567", "+49301234567", true},
		{"+49301234567", "+49301234568", false},
		{"+43301234567", "+49301234567", false}, // the same national number in another country: a different digit, not a match
		{"1234567", "+49301234567", false},      // a local number is not the whole number
		{"123456", "123456", false},             // too short to take for one
		{"", "", false},
		{"112", "112", false},
		{"+49301234567", "+49301234567", true},
		{"+4930123456789", "+49301234567", false}, // a longer number that merely starts alike
	} {
		if got := contacts.SameNumber(c.a, c.b); got != c.want {
			t.Errorf("SameNumber(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestNamesForCalls(t *testing.T) {
	pool := dbtest.Migrated(t)
	s := &contacts.Store{Pool: pool}
	var anna, ben, hs, hs2, book uuid.UUID
	pool.QueryRow(ctx, "INSERT INTO app_user (username, display_name, role, auth_source) VALUES ('anna', 'Anna', 'user', 'local') RETURNING id").Scan(&anna)
	pool.QueryRow(ctx, "INSERT INTO app_user (username, display_name, role, auth_source) VALUES ('ben', 'Ben', 'user', 'local') RETURNING id").Scan(&ben)
	pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\\x01', 1, '\\x') RETURNING id", anna).Scan(&hs)
	pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\\x02', 2, '\\x') RETURNING id", anna).Scan(&hs2)
	pool.QueryRow(ctx, "INSERT INTO addressbook (owner_user_id, label, url) VALUES ($1, 'Cloud', 'https://c.example.org/') RETURNING id", anna).Scan(&book)
	pool.Exec(ctx, "INSERT INTO handset_addressbook (handset_id, addressbook_id) VALUES ($1, $2)", hs, book)
	add := func(owner any, book any, last, first, number string) {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, "INSERT INTO contact (owner_user_id, last_name, first_name, addressbook_id, remote_href) VALUES ($1, $2, $3, $4, CASE WHEN $4::uuid IS NULL THEN NULL ELSE $2 END) RETURNING id", owner, last, first, book).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "INSERT INTO contact_number (contact_id, position, number, kind, is_default) VALUES ($1, 0, $2, 'other', true)", id, number); err != nil {
			t.Fatal(err)
		}
	}
	add(anna, nil, "Schmidt", "Anna", "+49301234567")
	add(anna, book, "Cloudy", "Carl", "01715551234")
	add(nil, nil, "Reception", "", "+4930777000")
	add(ben, nil, "Secret", "Ben", "0309998887")
	add(anna, book, "Doppelt", "Dora", "0301234567") // the same number as Schmidt: the user's own wins

	name := func(h uuid.UUID, n string) string {
		t.Helper()
		got, err := s.LookupName(ctx, h, n)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, c := range []struct {
		h    uuid.UUID
		n    string
		want string
	}{
		{hs, "0301234567", "Anna Schmidt"},
		{hs, "+49301234567", "Anna Schmidt"},
		{hs, "0049301234567", "Anna Schmidt"},
		{hs, "030 1234568", ""},
		{hs, "01715551234", "Carl Cloudy"}, // from the book ticked for this handset
		{hs2, "01715551234", ""},           // not ticked for the other
		{hs, "+4930777000", "Reception"},   // shared
		{hs2, "030777000", "Reception"},
		{hs, "0309998887", ""}, // another user's contact
		{hs, "112", ""},
		{hs, "", ""},
		{uuid.New(), "0301234567", ""}, // a handset that is gone
	} {
		if got := name(c.h, c.n); got != c.want {
			t.Errorf("LookupName(%q) = %q, want %q", c.n, got, c.want)
		}
	}
	// for the Calls page: all of the user's books count
	m, err := s.NamesFor(ctx, anna, []string{"01715551234", "0301234567", "+4930777000", "0309998887", "x"})
	if err != nil || len(m) != 3 || m["01715551234"] != "Carl Cloudy" || m["0301234567"] != "Anna Schmidt" || m["+4930777000"] != "Reception" {
		t.Errorf("NamesFor: %v %v", m, err)
	}
}
