// SPDX-License-Identifier: AGPL-3.0-or-later

package contacts

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

// The handsets' phonebook (docs/list-access.md). libopendectcell keeps a Contact List per record
// in the format of its dect_contacts.h: a contacts file in, a journal of what the handset
// changed out. This file turns the database's contacts into that file and a journal line
// back into a change of the handset owner's personal list.

const (
	// dectNums is how many numbers a handset's entry holds (DECT_CONTACT_NUMS); a contact may have
	// more in the database, and the handset never sees or saves them.
	dectNums = 3
	// dectName is the longest name in bytes (DECT_CONTACT_NAME_MAX less the NUL).
	dectName = 31
)

// 'Contact number' properties (dect_contacts.h): the kinds a handset can say.
var kindProps = map[string]uint8{"fixed": 0x08, "mobile": 0x04, "work": 0x02, "other": 0x00}

func propsKind(p uint8) string {
	switch {
	case p&0x08 != 0:
		return "fixed"
	case p&0x04 != 0:
		return "mobile"
	case p&0x02 != 0:
		return "work"
	}
	return "other"
}

// ordered is a contact's numbers as the handset sees them: the default first, then by position.
func ordered(ns []Number) []Number {
	out := append([]Number(nil), ns...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Default && !out[j].Default })
	return out
}

// cut shortens s to the handset's name length at a character.
func cut(s string) string {
	if len(s) <= dectName {
		return s
	}
	n := dectName
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// dectField makes a name or number safe for the file: no tab, no line break.
func dectField(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}

// digits is a number as the file holds it: 0-9 * # only, a leading + as 00.
func digits(n string) string {
	n = strings.Replace(n, "+", "00", 1)
	return strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || r == '*' || r == '#' {
			return r
		}
		return -1
	}, n)
}

// FileText is the Contact List file for a handset: the contacts it reads, with the journal
// of the handset's changes up to seq in it. hints maps a contact to the entry identifier the
// handset gave it when it made it, so that the handset keeps its entry (dect_contacts.h).
func FileText(cs []*Contact, seq uint32, hints map[int64]uint16) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#opendectcell-contacts 1 seq=%d\n", seq)
	for _, c := range cs {
		if c.ID <= 0 || c.ID > 0xFFFFFFFF {
			continue
		}
		hint := "-"
		if h := hints[c.ID]; h != 0 {
			hint = strconv.Itoa(int(h))
		}
		fmt.Fprintf(&b, "%08x\t%s\t%s\t%s\t0\t*", c.ID, hint, dectField(cut(c.LastName)), dectField(cut(c.FirstName)))
		n := 0
		for _, num := range ordered(c.Numbers) {
			d := digits(num.Number)
			if d == "" || len(d) > 31 || n == dectNums {
				continue
			}
			fmt.Fprintf(&b, "\t%02x:%s", kindProps[num.Kind], d)
			n++
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Change is one line of a handset's journal.
type Change struct {
	Seq   uint32
	Op    string // put, del, clear
	Entry uint16 // the handset's identifier for the entry
	Key   int64  // the contact's id here, 0 for an entry the handset made
	Last  string
	First string
	Nums  []Number
}

// ParseChange reads a journal line (dect_contacts.h).
func ParseChange(line string) (Change, error) {
	f := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
	var ch Change
	if len(f) < 2 {
		return ch, errors.New("contacts: short journal line")
	}
	seq, err := strconv.ParseUint(f[0], 10, 32)
	if err != nil {
		return ch, fmt.Errorf("contacts: journal sequence: %w", err)
	}
	ch.Seq, ch.Op = uint32(seq), f[1]
	switch ch.Op {
	case "clear":
		return ch, nil
	case "del", "put":
	default:
		return ch, fmt.Errorf("contacts: journal operation %q", ch.Op)
	}
	need := 4
	if ch.Op == "put" {
		need = 8
	}
	if len(f) < need {
		return ch, errors.New("contacts: short journal line")
	}
	id, err := strconv.ParseUint(f[2], 10, 16)
	if err != nil {
		return ch, fmt.Errorf("contacts: journal entry: %w", err)
	}
	key, err := strconv.ParseUint(f[3], 16, 32)
	if err != nil {
		return ch, fmt.Errorf("contacts: journal key: %w", err)
	}
	ch.Entry, ch.Key = uint16(id), int64(key)
	if ch.Op == "del" {
		return ch, nil
	}
	ch.Last, ch.First = f[4], f[5]
	for _, nf := range f[8:] {
		p, d, ok := strings.Cut(nf, ":")
		if !ok {
			continue
		}
		props, err := strconv.ParseUint(p, 16, 8)
		if err != nil || d == "" {
			continue
		}
		ch.Nums = append(ch.Nums, Number{Number: d, Kind: propsKind(uint8(props)), Default: len(ch.Nums) == 0})
	}
	return ch, nil
}

// ListFile is handset's Contact List as a file (FileText): its owner's contacts and the shared ones.
func (s *Store) ListFile(ctx context.Context, handset uuid.UUID, seq uint32, hints map[int64]uint16) (string, error) {
	cs, err := s.ForHandset(ctx, handset)
	if err != nil {
		return "", err
	}
	return FileText(cs, seq, hints), nil
}

// Applied is what ApplyChange did.
type Applied struct {
	Seq     uint32
	Contact int64  // the contact made or changed, 0 if none
	Entry   uint16 // the handset's identifier for it
}

// ErrNotApplied means a change that was understood and left alone (a shared contact, a
// "delete list", an entry with no name).
var ErrNotApplied = errors.New("contacts: the change was not applied")

// ApplyChange stores a change a handset made in its owner's personal list. A handset has no
// write access to the shared contacts, and may not empty the list.
func (s *Store) ApplyChange(ctx context.Context, handset uuid.UUID, line string) (Applied, error) {
	ch, err := ParseChange(line)
	if err != nil {
		return Applied{}, err
	}
	out := Applied{Seq: ch.Seq, Entry: ch.Entry}
	switch ch.Op {
	case "clear":
		return out, ErrNotApplied
	case "del":
		if ch.Key == 0 {
			return out, nil // a contact the handset made and deleted before the controller heard of it
		}
		return out, s.deleteFromHandset(ctx, handset, ch.Key)
	}
	c := Contact{LastName: strings.TrimSpace(ch.Last), FirstName: strings.TrimSpace(ch.First)}
	for _, n := range ch.Nums {
		n.Number = normalise(n.Number)
		if numberRE.MatchString(n.Number) {
			c.Numbers = append(c.Numbers, n)
		}
	}
	if len(c.Numbers) > 0 {
		c.Numbers[0].Default = true
	}
	if ch.Key != 0 {
		// what the handset cannot see stays: numbers beyond its three, a name it saw cut
		if cur, err := s.currentForHandset(ctx, handset, ch.Key); err == nil {
			if cut(cur.LastName) == c.LastName {
				c.LastName = cur.LastName
			}
			if cut(cur.FirstName) == c.FirstName {
				c.FirstName = cur.FirstName
			}
			if extra := ordered(cur.Numbers); len(extra) > dectNums {
				for _, n := range extra[dectNums:] {
					n.Default = false
					c.Numbers = append(c.Numbers, n)
				}
			}
		}
	}
	if c.LastName == "" && c.FirstName == "" {
		return out, ErrNotApplied
	}
	saved, err := s.SaveFromHandset(ctx, handset, ch.Key, c)
	if err != nil {
		return out, err
	}
	out.Contact = saved.ID
	return out, nil
}

func (s *Store) currentForHandset(ctx context.Context, handset uuid.UUID, id int64) (*Contact, error) {
	cs, err := s.ForHandset(ctx, handset)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) deleteFromHandset(ctx context.Context, handset uuid.UUID, id int64) error {
	var owner uuid.UUID
	if err := s.Pool.QueryRow(ctx, "SELECT owner_user_id FROM handset WHERE id = $1", handset).Scan(&owner); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	u, err := (&auth.Store{Pool: s.Pool}).UserByID(ctx, s.Pool, owner)
	if err != nil {
		return err
	}
	ctx = audit.WithActor(ctx, audit.Actor{Name: "handset:" + handset.String()})
	cur, err := s.Get(ctx, u, id)
	if err != nil {
		return err
	}
	if cur.OwnerID == nil {
		return ErrReadOnly // the shared contacts are read-only from a handset
	}
	return s.Delete(ctx, u, id)
}
