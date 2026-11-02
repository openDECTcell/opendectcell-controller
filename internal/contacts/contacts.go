// SPDX-License-Identifier: AGPL-3.0-or-later

// Package contacts holds the lists a CAT-iq handset reads (docs/list-access.md,
// #49): the phonebook, and the call lists drawn from the call log, scoped
// as the handset is entitled to see them.
package contacts

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

var (
	ErrNotFound  = errors.New("contacts: not found")
	ErrReadOnly  = errors.New("contacts: read-only for this handset")
	ErrNotMissed = errors.New("contacts: not a missed call")
)

// ValidationError lists problems by field.
type ValidationError map[string]string

func (v ValidationError) Error() string {
	var parts []string
	for k, m := range v {
		parts = append(parts, k+": "+m)
	}
	return "invalid contact: " + strings.Join(parts, "; ")
}

// Kinds of number.
var kinds = map[string]bool{"fixed": true, "mobile": true, "work": true, "other": true}

// MaxNumbers per contact (docs/list-access.md §2).
const MaxNumbers = 5

// Number is one of a contact's numbers.
type Number struct {
	Number  string `json:"number"`
	Kind    string `json:"kind"`
	Default bool   `json:"is_default"`
}

// Contact is a phonebook entry. OwnerID nil means shared.
type Contact struct {
	ID        int64      `json:"id"`
	OwnerID   *uuid.UUID `json:"owner_id"`
	LastName  string     `json:"last_name"`
	FirstName string     `json:"first_name"`
	Numbers   []Number   `json:"numbers"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Store holds contacts.
type Store struct {
	Pool *pgxpool.Pool
	// OnChange, if set, is told after a contact was created, changed or deleted: whose list it
	// is (nil: a shared contact, every handset reads it). The DECT side refreshes the
	// handsets' phonebooks with it.
	OnChange func(owner *uuid.UUID)
}

func (s *Store) changed(owner *uuid.UUID) {
	if s.OnChange != nil {
		s.OnChange(owner)
	}
}

var numberRE = regexp.MustCompile(`^\+?[0-9*#]{1,32}$`)

// normalise strips the separators people type ("030 123-45 67").
func normalise(n string) string {
	return strings.NewReplacer(" ", "", "-", "", "/", "", "(", "", ")", "", ".", "").Replace(strings.TrimSpace(n))
}

func validate(c *Contact) error {
	v := ValidationError{}
	c.LastName, c.FirstName = strings.TrimSpace(c.LastName), strings.TrimSpace(c.FirstName)
	if c.LastName == "" && c.FirstName == "" {
		v["name"] = "a first or last name is required"
	}
	if len(c.LastName) > 64 || len(c.FirstName) > 64 {
		v["name"] = "at most 64 characters each"
	}
	if len(c.Numbers) > MaxNumbers {
		v["numbers"] = fmt.Sprintf("at most %d", MaxNumbers)
	}
	defaults := 0
	for i := range c.Numbers {
		n := &c.Numbers[i]
		n.Number = normalise(n.Number)
		if n.Kind == "" {
			n.Kind = "other"
		}
		if !numberRE.MatchString(n.Number) {
			v[fmt.Sprintf("numbers[%d].number", i)] = "digits, an optional leading +, * and #"
		}
		if !kinds[n.Kind] {
			v[fmt.Sprintf("numbers[%d].kind", i)] = "fixed, mobile, work or other"
		}
		if n.Default {
			defaults++
		}
	}
	if defaults > 1 {
		v["numbers"] = "at most one default number"
	}
	if defaults == 0 && len(c.Numbers) > 0 {
		c.Numbers[0].Default = true
	}
	if len(v) > 0 {
		return v
	}
	return nil
}

// canRead: own contacts, shared contacts, everything for admins.
func canRead(u *auth.User, owner *uuid.UUID) bool {
	return u != nil && !u.Disabled && (owner == nil || auth.CanAccess(u, owner))
}

// load fills in numbers for contacts read by q.
func (s *Store) load(ctx context.Context, q string, args ...any) ([]*Contact, error) {
	rows, err := s.Pool.Query(ctx, `SELECT c.id, c.owner_user_id, c.last_name, c.first_name, c.created_at, c.updated_at,
		coalesce(json_agg(json_build_object('number', n.number, 'kind', n.kind, 'is_default', n.is_default) ORDER BY n.position)
		         FILTER (WHERE n.number IS NOT NULL), '[]')
		FROM contact c LEFT JOIN contact_number n ON n.contact_id = c.id WHERE `+q+`
		GROUP BY c.id ORDER BY lower(c.last_name), lower(c.first_name), c.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Contact
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.OwnerID, &c.LastName, &c.FirstName, &c.CreatedAt, &c.UpdatedAt, &c.Numbers); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// List returns the contacts u may read. scope "personal" or "shared"
// narrows it; admins may pass owner (a user id) to see that user's.
func (s *Store) List(ctx context.Context, u *auth.User, scope, owner string) ([]*Contact, error) {
	if u == nil {
		return nil, auth.ErrForbidden
	}
	me := u.ID
	if owner != "" && u.IsAdmin() {
		id, err := uuid.Parse(owner)
		if err != nil {
			return nil, ValidationError{"owner": "a user id"}
		}
		me = id
	}
	switch scope {
	case "personal":
		return s.load(ctx, "c.owner_user_id = $1", me)
	case "shared":
		return s.load(ctx, "c.owner_user_id IS NULL")
	case "":
		return s.load(ctx, "(c.owner_user_id = $1 OR c.owner_user_id IS NULL)", me)
	}
	return nil, ValidationError{"scope": "personal or shared"}
}

// Get returns one contact u may read.
func (s *Store) Get(ctx context.Context, u *auth.User, id int64) (*Contact, error) {
	cs, err := s.load(ctx, "c.id = $1", id)
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 || !canRead(u, cs[0].OwnerID) {
		return nil, ErrNotFound
	}
	return cs[0], nil
}

func writeNumbers(ctx context.Context, tx pgx.Tx, id int64, ns []Number) error {
	if _, err := tx.Exec(ctx, "DELETE FROM contact_number WHERE contact_id = $1", id); err != nil {
		return err
	}
	for i, n := range ns {
		if _, err := tx.Exec(ctx, "INSERT INTO contact_number (contact_id, position, number, kind, is_default) VALUES ($1, $2, $3, $4, $5)",
			id, i, n.Number, n.Kind, n.Default); err != nil {
			return err
		}
	}
	return nil
}

func scope(owner *uuid.UUID) string {
	if owner == nil {
		return "shared"
	}
	return "personal"
}

// Create adds a contact. It is u's own unless an admin sets owner
// (setOwner true; owner nil = shared).
func (s *Store) Create(ctx context.Context, u *auth.User, c Contact, setOwner bool) (*Contact, error) {
	if u == nil {
		return nil, auth.ErrForbidden
	}
	owner := &u.ID
	if setOwner {
		if !u.IsAdmin() {
			return nil, auth.ErrForbidden
		}
		owner = c.OwnerID
	}
	if err := validate(&c); err != nil {
		return nil, err
	}
	var id int64
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "INSERT INTO contact (owner_user_id, last_name, first_name) VALUES ($1, $2, $3) RETURNING id",
			owner, c.LastName, c.FirstName).Scan(&id); err != nil {
			return err
		}
		if err := writeNumbers(ctx, tx, id, c.Numbers); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "contact.create", ObjectType: "contact", ObjectID: fmt.Sprint(id),
			Details: map[string]any{"scope": scope(owner)}})
	})
	if err != nil {
		return nil, err
	}
	s.changed(owner)
	return s.Get(ctx, u, id)
}

// Update replaces a contact's names and numbers. Writing needs ownership
// (shared contacts: admins).
func (s *Store) Update(ctx context.Context, u *auth.User, id int64, c Contact) (*Contact, error) {
	old, err := s.Get(ctx, u, id)
	if err != nil {
		return nil, err
	}
	if !auth.CanAccess(u, old.OwnerID) {
		return nil, auth.ErrForbidden
	}
	if err := validate(&c); err != nil {
		return nil, err
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE contact SET last_name = $2, first_name = $3, updated_at = now() WHERE id = $1",
			id, c.LastName, c.FirstName); err != nil {
			return err
		}
		if err := writeNumbers(ctx, tx, id, c.Numbers); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "contact.update", ObjectType: "contact", ObjectID: fmt.Sprint(id)})
	})
	if err != nil {
		return nil, err
	}
	s.changed(old.OwnerID)
	return s.Get(ctx, u, id)
}

// Delete removes a contact u may write.
func (s *Store) Delete(ctx context.Context, u *auth.User, id int64) error {
	old, err := s.Get(ctx, u, id)
	if err != nil {
		return err
	}
	if !auth.CanAccess(u, old.OwnerID) {
		return auth.ErrForbidden
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM contact WHERE id = $1", id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "contact.delete", ObjectType: "contact", ObjectID: fmt.Sprint(id),
			Details: map[string]any{"scope": scope(old.OwnerID)}})
	})
	if err == nil {
		s.changed(old.OwnerID)
	}
	return err
}

// handset views (what the DECT side will serve)

// ForHandset returns the contacts a handset reads: its owner's and the
// shared ones.
func (s *Store) ForHandset(ctx context.Context, handset uuid.UUID) ([]*Contact, error) {
	var owner uuid.UUID
	if err := s.Pool.QueryRow(ctx, "SELECT owner_user_id FROM handset WHERE id = $1", handset).Scan(&owner); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return s.load(ctx, "(c.owner_user_id = $1 OR c.owner_user_id IS NULL)", owner)
}

// SaveFromHandset stores a contact edited on a handset in its owner's
// personal list (id 0 creates). A handset has no write access to the
// shared contacts.
func (s *Store) SaveFromHandset(ctx context.Context, handset uuid.UUID, id int64, c Contact) (*Contact, error) {
	var owner uuid.UUID
	if err := s.Pool.QueryRow(ctx, "SELECT owner_user_id FROM handset WHERE id = $1", handset).Scan(&owner); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	u, err := (&auth.Store{Pool: s.Pool}).UserByID(ctx, s.Pool, owner)
	if err != nil {
		return nil, err
	}
	ctx = audit.WithActor(ctx, audit.Actor{Name: "handset:" + handset.String()})
	if id == 0 {
		return s.Create(ctx, u, c, false)
	}
	cur, err := s.Get(ctx, u, id)
	if err != nil {
		return nil, err
	}
	if cur.OwnerID == nil {
		return nil, ErrReadOnly // a handset edits only its owner's list
	}
	return s.Update(ctx, u, id, c)
}

// CallList names a call list.
type CallList string

const (
	Missed           CallList = "missed_calls"
	IncomingAccepted CallList = "incoming_accepted_calls"
	AllIncoming      CallList = "all_incoming_calls"
	Outgoing         CallList = "outgoing_calls"
)

// CallEntry is one call-list entry.
type CallEntry struct {
	ID         int64      `json:"id"`
	Number     string     `json:"number"`
	Name       string     `json:"name"` // from the contacts the handset reads, if any
	StartedAt  time.Time  `json:"started_at"`
	Line       *int16     `json:"line"`
	Answered   bool       `json:"answered"`
	MissedRead *time.Time `json:"missed_read_at"`
	SIPAccount *uuid.UUID `json:"sip_account_id"`
}

// CallsForHandset returns a call list as the handset sees it
// (docs/list-access.md §1): incoming lists by the SIP accounts on its
// lines, outgoing by the handset itself. Newest first, at most limit.
func (s *Store) CallsForHandset(ctx context.Context, handset uuid.UUID, list CallList, limit int) ([]CallEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var where string
	switch list {
	case Missed:
		where = "cl.direction = 'in' AND cl.answered_at IS NULL AND cl.sip_account_id IN (SELECT sip_account_id FROM handset_line WHERE handset_id = $1)"
	case IncomingAccepted:
		where = "cl.direction = 'in' AND cl.answered_at IS NOT NULL AND cl.sip_account_id IN (SELECT sip_account_id FROM handset_line WHERE handset_id = $1)"
	case AllIncoming:
		where = "cl.direction = 'in' AND cl.sip_account_id IN (SELECT sip_account_id FROM handset_line WHERE handset_id = $1)"
	case Outgoing:
		where = "cl.direction = 'out' AND cl.handset_id = $1"
	default:
		return nil, ValidationError{"list": "missed_calls, incoming_accepted_calls, all_incoming_calls or outgoing_calls"}
	}
	var owner *uuid.UUID
	if err := s.Pool.QueryRow(ctx, "SELECT owner_user_id FROM handset WHERE id = $1", handset).Scan(&owner); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	// The name comes from the contacts this handset may read.
	rows, err := s.Pool.Query(ctx, `SELECT cl.id, cl.remote_party, cl.started_at, cl.line, cl.answered_at IS NOT NULL, cl.missed_read_at, cl.sip_account_id,
		coalesce((SELECT trim(c.first_name || ' ' || c.last_name) FROM contact c JOIN contact_number n ON n.contact_id = c.id
		          WHERE n.number = cl.remote_party AND (c.owner_user_id IS NULL OR c.owner_user_id = $2)
		          ORDER BY c.owner_user_id NULLS LAST LIMIT 1), '')
		FROM call_log cl WHERE `+where+` ORDER BY cl.started_at DESC LIMIT $3`, handset, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallEntry
	for rows.Next() {
		var e CallEntry
		if err := rows.Scan(&e.ID, &e.Number, &e.StartedAt, &e.Line, &e.Answered, &e.MissedRead, &e.SIPAccount, &e.Name); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkMissedRead sets a missed call's shared read mark (one per call,
// docs/list-access.md §2). u must be the call's owner or an admin.
func (s *Store) MarkMissedRead(ctx context.Context, u *auth.User, call int64) error {
	var owner *uuid.UUID
	var dir string
	var answered bool
	err := s.Pool.QueryRow(ctx, "SELECT owner_user_id, direction, answered_at IS NOT NULL FROM call_log WHERE id = $1", call).Scan(&owner, &dir, &answered)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !auth.CanAccess(u, owner)) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if dir != "in" || answered {
		return ErrNotMissed
	}
	_, err = s.Pool.Exec(ctx, "UPDATE call_log SET missed_read_at = coalesce(missed_read_at, now()) WHERE id = $1", call)
	return err
}
