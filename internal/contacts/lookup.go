// SPDX-License-Identifier: AGPL-3.0-or-later

package contacts

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// matchDigits is the digits of a number without the zeros in front: "+49 30 1234567", "0049301234567" and
// "030 1234567" end in the same digits, and the zeros (the international prefix, the national prefix) are what
// differs.
func matchDigits(n string) string {
	var b strings.Builder
	for _, r := range n {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "0")
}

// minMatch is how many digits two numbers must share before they are taken for one: fewer would match a short
// extension to anything.
const minMatch = 7

// SameNumber says whether two ways of writing a number are one number: the same digits, or one with a country code
// (at most three digits more) in front of the other's national number.
func SameNumber(a, b string) bool {
	x, y := matchDigits(a), matchDigits(b)
	if len(x) > len(y) {
		x, y = y, x
	}
	if len(x) < minMatch {
		return false
	}
	return len(y)-len(x) <= 3 && strings.HasSuffix(y, x)
}

// tail is the last seven digits of a number, what the database index holds.
func tail(n string) string {
	d := matchDigits(n)
	if len(d) < minMatch {
		return ""
	}
	return d[len(d)-minMatch:]
}

func displayName(c *Contact) string {
	return strings.TrimSpace(c.FirstName + " " + c.LastName)
}

// lookup finds the contact among those that q selects (with $1 the owner and, where the query takes it, $2 the
// handset) whose number is one of numbers. It returns names by the number as it was asked for.
func (s *Store) lookup(ctx context.Context, where string, args []any, numbers []string) (map[string]string, error) {
	var tails []string
	asked := map[string][]string{} // tail → numbers asked
	for _, n := range numbers {
		if t := tail(n); t != "" {
			if _, ok := asked[t]; !ok {
				tails = append(tails, t)
			}
			asked[t] = append(asked[t], n)
		}
	}
	out := map[string]string{}
	if len(tails) == 0 {
		return out, nil
	}
	args = append(args, tails)
	rows, err := s.Pool.Query(ctx, `SELECT c.id, c.last_name, c.first_name, n.number FROM contact c JOIN contact_number n ON n.contact_id = c.id
		WHERE (`+where+`) AND n.match_tail = ANY($`+strconv.Itoa(len(args))+`)
		ORDER BY (c.addressbook_id IS NOT NULL), lower(c.last_name), lower(c.first_name), c.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Contact
		var num string
		if err := rows.Scan(&c.ID, &c.LastName, &c.FirstName, &num); err != nil {
			return nil, err
		}
		for _, a := range asked[tail(num)] {
			if _, done := out[a]; !done && SameNumber(a, num) {
				if name := displayName(&c); name != "" {
					out[a] = name
				}
			}
		}
	}
	return out, rows.Err()
}

// LookupName is the name of the contact a handset knows under number: among its owner's own, the shared ones and the
// address books ticked for it, "" if none. A contact of the owner's own wins over a shared one and over a book's.
func (s *Store) LookupName(ctx context.Context, handset uuid.UUID, number string) (string, error) {
	var owner uuid.UUID
	if err := s.Pool.QueryRow(ctx, "SELECT owner_user_id FROM handset WHERE id = $1", handset).Scan(&owner); errors.Is(err, pgx.ErrNoRows) {
		return "", nil // a handset that is gone has no contacts
	} else if err != nil {
		return "", err
	}
	m, err := s.lookup(ctx, `(c.owner_user_id = $1 OR c.owner_user_id IS NULL)
		AND (c.addressbook_id IS NULL OR c.addressbook_id IN (SELECT addressbook_id FROM handset_addressbook WHERE handset_id = $2))`,
		[]any{owner, handset}, []string{number})
	return m[number], err
}

// NamesFor gives the names of the contacts an owner has (their own, the shared ones and all their address books)
// for the numbers asked; numbers without a contact are not in the result.
func (s *Store) NamesFor(ctx context.Context, owner uuid.UUID, numbers []string) (map[string]string, error) {
	return s.lookup(ctx, `c.owner_user_id = $1 OR c.owner_user_id IS NULL`, []any{owner}, numbers)
}
