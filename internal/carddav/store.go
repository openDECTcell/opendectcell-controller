// SPDX-License-Identifier: AGPL-3.0-or-later

package carddav

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

// MaxBooksPerUser bounds what one user links.
const MaxBooksPerUser = 10

// ErrNotFound means no such book, or one of somebody else's.
var ErrNotFound = errors.New("carddav: not found")

// ValidationError names the fields that are wrong, in words.
type ValidationError map[string]string

func (v ValidationError) Error() string {
	var parts []string
	for k, m := range v {
		parts = append(parts, k+": "+m)
	}
	return strings.Join(parts, "; ")
}

// Fetcher reads a book; Client is the one that talks to a server, a test brings its own.
type Fetcher interface {
	Fetch(ctx context.Context, url, user, password string) (Result, error)
}

// Book is a linked address book. The password is never in it.
type Book struct {
	ID          uuid.UUID
	OwnerID     uuid.UUID
	Label       string
	URL         string
	Username    string
	HasPassword bool
	Enabled     bool
	IntervalMin int
	LastSyncAt  *time.Time
	LastOKAt    *time.Time
	LastError   string
	LastNote    string
	Count       int
	CreatedAt   time.Time
}

// Input is what a user gives for a book.
type Input struct {
	Label       string
	URL         string
	Username    string
	Password    string // "" keeps the stored one on an update
	Enabled     bool
	IntervalMin int
}

// Store holds the books and runs their sync.
type Store struct {
	Pool    *pgxpool.Pool
	Keyring *secret.Keyring
	Client  Fetcher
	// OnChange is told whose phonebooks changed after a sync changed contacts (the handsets' lists are refreshed).
	OnChange func(owner uuid.UUID)
	Now      func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

const bookCols = `id, owner_user_id, label, url, username, password_enc IS NOT NULL, enabled, interval_min, last_sync_at, last_ok_at,
	last_error, last_note, contact_count, created_at`

func scanBook(r pgx.Row) (*Book, error) {
	var b Book
	err := r.Scan(&b.ID, &b.OwnerID, &b.Label, &b.URL, &b.Username, &b.HasPassword, &b.Enabled, &b.IntervalMin, &b.LastSyncAt, &b.LastOKAt,
		&b.LastError, &b.LastNote, &b.Count, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &b, err
}

func loc(id uuid.UUID) secret.Location {
	return secret.Location{Table: "addressbook", Column: "password_enc", RowID: id.String()}
}

func clean(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
}

func (in *Input) validate() error {
	v := ValidationError{}
	in.Label, in.URL, in.Username = clean(in.Label), clean(in.URL), clean(in.Username)
	if in.Label == "" || len(in.Label) > 64 {
		v["label"] = "a name of 1 to 64 characters"
	}
	if err := CheckURL(in.URL); err != nil {
		v["url"] = err.Error()
	} else if len(in.URL) > 512 {
		v["url"] = "at most 512 characters"
	}
	if len(in.Username) > 128 {
		v["username"] = "at most 128 characters"
	}
	if len(in.Password) > 256 {
		v["password"] = "at most 256 characters"
	}
	if in.IntervalMin == 0 {
		in.IntervalMin = 60
	}
	if in.IntervalMin < 5 || in.IntervalMin > 1440 {
		v["interval"] = "between 5 minutes and 24 hours"
	}
	if len(v) > 0 {
		return v
	}
	return nil
}

// Create links a book for u. attach lists the handsets of u that show it at once (nil: none).
func (s *Store) Create(ctx context.Context, u *auth.User, in Input, attach []uuid.UUID) (*Book, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	var b *Book
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM addressbook WHERE owner_user_id = $1", u.ID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxBooksPerUser {
			return ValidationError{"label": fmt.Sprintf("at most %d address books", MaxBooksPerUser)}
		}
		var id uuid.UUID
		err := tx.QueryRow(ctx, `INSERT INTO addressbook (owner_user_id, label, url, username, enabled, interval_min) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (owner_user_id, label) DO NOTHING RETURNING id`, u.ID, in.Label, in.URL, in.Username, in.Enabled, in.IntervalMin).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ValidationError{"label": "you have an address book with that name"}
		}
		if err != nil {
			return err
		}
		if in.Password != "" {
			enc, err := s.Keyring.Seal(loc(id), []byte(in.Password))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE addressbook SET password_enc = $2 WHERE id = $1", id, enc); err != nil {
				return err
			}
		}
		for _, h := range attach {
			if _, err := tx.Exec(ctx, `INSERT INTO handset_addressbook (handset_id, addressbook_id)
				SELECT id, $2 FROM handset WHERE id = $1 AND owner_user_id = $3 ON CONFLICT DO NOTHING`, h, id, u.ID); err != nil {
				return err
			}
		}
		if err := audit.Record(ctx, tx, audit.Entry{Action: "addressbook.create", ObjectType: "addressbook", ObjectID: id.String(),
			Details: map[string]any{"label": in.Label, "host": hostOf(in.URL)}}); err != nil {
			return err
		}
		b, err = scanBook(tx.QueryRow(ctx, "SELECT "+bookCols+" FROM addressbook WHERE id = $1", id))
		return err
	})
	return b, err
}

// hostOf is the host of a book's address, for the audit log: the path may name the user.
func hostOf(u string) string {
	u = strings.TrimPrefix(u, "https://")
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

// List returns u's books.
func (s *Store) List(ctx context.Context, u *auth.User) ([]*Book, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+bookCols+" FROM addressbook WHERE owner_user_id = $1 ORDER BY lower(label)", u.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Book, error) { return scanBook(r) })
}

// Get returns one of u's books.
func (s *Store) Get(ctx context.Context, u *auth.User, id uuid.UUID) (*Book, error) {
	b, err := scanBook(s.Pool.QueryRow(ctx, "SELECT "+bookCols+" FROM addressbook WHERE id = $1 AND owner_user_id = $2", id, u.ID))
	return b, err
}

// Delete unlinks a book; the contacts that came from it go with it.
func (s *Store) Delete(ctx context.Context, u *auth.User, id uuid.UUID) error {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "DELETE FROM addressbook WHERE id = $1 AND owner_user_id = $2", id, u.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "addressbook.delete", ObjectType: "addressbook", ObjectID: id.String()})
	})
	if err == nil && s.OnChange != nil {
		s.OnChange(u.ID)
	}
	return err
}

// Update changes a book of u's. An empty password keeps the stored one.
func (s *Store) Update(ctx context.Context, u *auth.User, id uuid.UUID, in Input) (*Book, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	var b *Book
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE addressbook SET label = $3, url = $4, username = $5, enabled = $6, interval_min = $7
			WHERE id = $1 AND owner_user_id = $2`, id, u.ID, in.Label, in.URL, in.Username, in.Enabled, in.IntervalMin)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return ValidationError{"label": "you have an address book with that name"}
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if in.Password != "" {
			enc, err := s.Keyring.Seal(loc(id), []byte(in.Password))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE addressbook SET password_enc = $2 WHERE id = $1", id, enc); err != nil {
				return err
			}
		}
		if err := audit.Record(ctx, tx, audit.Entry{Action: "addressbook.update", ObjectType: "addressbook", ObjectID: id.String(),
			Details: map[string]any{"label": in.Label, "host": hostOf(in.URL), "login_changed": in.Password != ""}}); err != nil {
			return err
		}
		b, err = scanBook(tx.QueryRow(ctx, "SELECT "+bookCols+" FROM addressbook WHERE id = $1", id))
		return err
	})
	return b, err
}

// SetEnabled switches a book's sync on or off.
func (s *Store) SetEnabled(ctx context.Context, u *auth.User, id uuid.UUID, on bool) error {
	tag, err := s.Pool.Exec(ctx, "UPDATE addressbook SET enabled = $3 WHERE id = $1 AND owner_user_id = $2", id, u.ID, on)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// SetHandsetBooks makes the books a handset shows exactly books (the handset and the books are u's; others are
// ignored). It runs inside the caller's transaction.
func SetHandsetBooks(ctx context.Context, tx pgx.Tx, owner, handset uuid.UUID, books []uuid.UUID) error {
	if _, err := tx.Exec(ctx, "DELETE FROM handset_addressbook WHERE handset_id = $1", handset); err != nil {
		return err
	}
	for _, b := range books {
		if _, err := tx.Exec(ctx, `INSERT INTO handset_addressbook (handset_id, addressbook_id)
			SELECT $1, id FROM addressbook WHERE id = $2 AND owner_user_id = $3 ON CONFLICT DO NOTHING`, handset, b, owner); err != nil {
			return err
		}
	}
	return nil
}

// HandsetBooks lists the books each of the handsets shows.
func (s *Store) HandsetBooks(ctx context.Context, handset uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.Pool.Query(ctx, "SELECT addressbook_id FROM handset_addressbook WHERE handset_id = $1", handset)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// Sync reads one book now and brings its contacts in step: new cards are added, changed ones (by the server's etag)
// updated, vanished ones removed. A book that cannot be read keeps the contacts it has and says why.
func (s *Store) Sync(ctx context.Context, id uuid.UUID) error {
	var owner uuid.UUID
	var url, user string
	var enc []byte
	if err := s.Pool.QueryRow(ctx, "SELECT owner_user_id, url, username, password_enc FROM addressbook WHERE id = $1", id).Scan(&owner, &url, &user, &enc); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	pw := ""
	if enc != nil {
		p, err := s.Keyring.Open(loc(id), enc)
		if err != nil {
			return s.fail(ctx, id, errors.New("the stored password cannot be opened"))
		}
		pw = string(p)
	}
	res, err := s.Client.Fetch(ctx, url, user, pw)
	if err != nil {
		return s.fail(ctx, id, err)
	}
	changed, err := s.apply(ctx, id, owner, res)
	if err != nil {
		return err
	}
	if changed && s.OnChange != nil {
		s.OnChange(owner)
	}
	return nil
}

func (s *Store) fail(ctx context.Context, id uuid.UUID, cause error) error {
	msg := cause.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	_, err := s.Pool.Exec(ctx, "UPDATE addressbook SET last_sync_at = $2, last_error = $3 WHERE id = $1", id, s.now(), msg)
	if err != nil {
		return err
	}
	return cause
}

func (s *Store) apply(ctx context.Context, book, owner uuid.UUID, res Result) (bool, error) {
	changed := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		type have struct {
			id   int64
			etag string
		}
		existing := map[string]have{}
		rows, err := tx.Query(ctx, "SELECT id, remote_href, coalesce(remote_etag, '') FROM contact WHERE addressbook_id = $1", book)
		if err != nil {
			return err
		}
		for rows.Next() {
			var h have
			var href string
			if err := rows.Scan(&h.id, &href, &h.etag); err != nil {
				rows.Close()
				return err
			}
			existing[href] = h
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, c := range res.Cards {
			if seen[c.Href] {
				continue
			}
			seen[c.Href] = true
			h, ok := existing[c.Href]
			if ok && c.ETag != "" && c.ETag == h.etag {
				continue
			}
			changed = true
			id := h.id
			if ok {
				if _, err := tx.Exec(ctx, "UPDATE contact SET last_name = $2, first_name = $3, remote_etag = nullif($4, ''), updated_at = now() WHERE id = $1",
					id, c.Contact.LastName, c.Contact.FirstName, c.ETag); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "DELETE FROM contact_number WHERE contact_id = $1", id); err != nil {
					return err
				}
			} else if err := tx.QueryRow(ctx, `INSERT INTO contact (owner_user_id, last_name, first_name, addressbook_id, remote_href, remote_etag)
				VALUES ($1, $2, $3, $4, $5, nullif($6, '')) RETURNING id`, owner, c.Contact.LastName, c.Contact.FirstName, book, c.Href, c.ETag).Scan(&id); err != nil {
				return err
			}
			for i, n := range c.Contact.Numbers {
				if _, err := tx.Exec(ctx, "INSERT INTO contact_number (contact_id, position, number, kind, is_default) VALUES ($1, $2, $3, $4, $5)",
					id, i, n.Number, n.Kind, i == 0); err != nil {
					return err
				}
			}
		}
		for href, h := range existing {
			if !seen[href] {
				changed = true
				if _, err := tx.Exec(ctx, "DELETE FROM contact WHERE id = $1", h.id); err != nil {
					return err
				}
			}
		}
		note := ""
		if len(res.Skipped) > 0 {
			note = fmt.Sprintf("%d could not be used: %s", len(res.Skipped), res.Skipped[0])
			if len(note) > 300 {
				note = note[:300]
			}
		}
		_, err = tx.Exec(ctx, `UPDATE addressbook SET last_sync_at = $2, last_ok_at = $2, last_error = '', last_note = $3, contact_count = $4 WHERE id = $1`,
			book, s.now(), note, len(res.Cards))
		return err
	})
	return changed, err
}

// Due lists the enabled books whose time has come.
func (s *Store) Due(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM addressbook WHERE enabled AND (last_sync_at IS NULL OR last_sync_at + make_interval(mins => interval_min) <= $1)
		ORDER BY last_sync_at NULLS FIRST LIMIT 20`, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// Run syncs the books that are due, once a minute, until ctx ends. A book that fails is tried again at its
// interval, not every minute.
func (s *Store) Run(ctx context.Context, log func(msg string, args ...any)) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			ids, err := s.Due(ctx, now)
			if err != nil {
				log("address books: due", "err", err)
				continue
			}
			for _, id := range ids {
				cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				if err := s.Sync(cctx, id); err != nil && !errors.Is(err, ErrNotFound) {
					log("address book not synced", "book", id, "err", err)
				}
				cancel()
			}
		}
	}
}
