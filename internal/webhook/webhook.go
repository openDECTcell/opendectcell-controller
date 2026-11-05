// SPDX-License-Identifier: AGPL-3.0-or-later

// Package webhook posts events to addresses the users gave (ideas F2): signed, retried, and kept away from the controller's own
// network. A user's webhooks get that user's events only. A delivery is a row in webhook_delivery, so a restart loses none, and the
// worker (Dispatcher) takes due rows, posts them, and reschedules or gives up.
package webhook

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/netguard"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

// The limits.
const (
	MaxPerUser = 10  // webhooks a user may have
	MaxPending = 500 // deliveries waiting for one webhook: past that the oldest are dropped
	FailLimit  = 10  // deliveries in a row that ended as failed: then the webhook is switched off
	KeepDays   = 7   // how long finished deliveries are kept, for the log on the webhook's page
)

// Event is something a webhook can be told.
type Event struct {
	Name  string
	Title string
}

// Events are the events a webhook can subscribe to. "ping" is the test event: it is not in Events, a user cannot subscribe to it, and a test sends it to one webhook only.
var Events = []Event{
	{"call.incoming", "A call comes in on one of my handsets"},
	{"call.missed", "I miss a call"},
	{"call.ended", "A call ends"},
	{"handset.online", "A handset of mine is heard again"},
	{"handset.offline", "A handset of mine is not heard any more"},
	{"notification", "The notification centre tells me something"},
}

// ValidEvent says whether name is an event there is.
func ValidEvent(name string) bool {
	return slices.ContainsFunc(Events, func(e Event) bool { return e.Name == name })
}

// Formats: the posted body. json is the envelope below; ntfy is a plain text message with Title and Tags headers, which ntfy.sh
// and Home Assistant's notify understand.
const (
	FormatJSON = "json"
	FormatNtfy = "ntfy"
)

var (
	// ErrNotFound means no such webhook of the user.
	ErrNotFound = errors.New("webhook: not found")
)

// ValidationError is a refused input, with a message for the person.
type ValidationError string

func (v ValidationError) Error() string { return string(v) }

// Hook is a webhook as the pages show it. It never holds the secret.
type Hook struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	Name           string
	URL            string
	Format         string
	Events         []string
	Enabled        bool
	HasAuth        bool
	CreatedAt      time.Time
	Failures       int
	DisabledReason string
	LastAt         *time.Time
	LastStatus     string
}

// Input makes or changes a webhook. Auth is the value of an Authorization header sent with every post ("Bearer ..."): on a change
// "" keeps what is stored and ClearAuth removes it.
type Input struct {
	Name      string
	URL       string
	Format    string
	Events    []string
	Enabled   bool
	Auth      string
	ClearAuth bool
}

// Store keeps the webhooks and their deliveries.
type Store struct {
	Pool    *pgxpool.Pool
	Keyring *secret.Keyring
	// Policy says which addresses may be used (netguard). Where it allows private networks, plain http is allowed too (a Home Assistant
	// on the home network); otherwise only https.
	Policy netguard.Policy
	Now    func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func loc(id uuid.UUID, col string) secret.Location {
	return secret.Location{Table: "webhook", Column: col, RowID: id.String()}
}

// CheckURL says whether raw is an address a webhook may post to, as far as can be told without connecting: https (or http where private
// networks are allowed), a host, no credentials in the address, no fragment, and an address literal that is not one the controller
// never connects to. Names are checked when the connection is made, after they are resolved.
func (s *Store) CheckURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.Hostname() == "":
		return ValidationError("That is not an address a webhook can post to. Write it like https://example.org/hook.")
	case len(raw) > 500:
		return ValidationError("The address is longer than 500 characters.")
	case u.User != nil:
		return ValidationError("Do not put a user name or password in the address. Use the Authorization field for a token.")
	case u.Fragment != "":
		return ValidationError("The address has a # part, which is never sent.")
	case u.Scheme != "https" && (u.Scheme != "http" || !s.Policy.AllowPrivate):
		if u.Scheme == "http" {
			return ValidationError("The address must start with https://. Plain http is allowed only where the operator has allowed private networks (OPENDECTCELL_WEBHOOK_ALLOW_PRIVATE).")
		}
		return ValidationError("The address must start with https://.")
	}
	if strings.ContainsFunc(raw, unicode.IsControl) || strings.ContainsAny(raw, " \t") {
		return ValidationError("The address has a space or a control character in it.")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		if err := s.Policy.Control("tcp", netip.AddrPortFrom(ip, 443).String(), nil); err != nil {
			return ValidationError("That address is on this machine or its own network, which the controller does not post to.")
		}
	}
	return nil
}

func (s *Store) validate(in *Input) error {
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	if in.Name == "" || len(in.Name) > 64 || strings.ContainsFunc(in.Name, unicode.IsControl) {
		return ValidationError("Give the webhook a name of up to 64 characters.")
	}
	if in.Format == "" {
		in.Format = FormatJSON
	}
	if in.Format != FormatJSON && in.Format != FormatNtfy {
		return ValidationError("The format is json or ntfy.")
	}
	in.Events = slices.Clone(in.Events)
	slices.Sort(in.Events)
	in.Events = slices.Compact(in.Events)
	if len(in.Events) == 0 {
		return ValidationError("Choose at least one event.")
	}
	for _, e := range in.Events {
		if !ValidEvent(e) {
			return ValidationError("Unknown event " + fmt.Sprintf("%q", e) + ".")
		}
	}
	if len(in.Auth) > 300 || strings.ContainsFunc(in.Auth, func(r rune) bool { return unicode.IsControl(r) }) {
		return ValidationError("The Authorization value is too long, or has a line break in it.")
	}
	return s.CheckURL(in.URL)
}

// newSecret is a signing secret: "whsec_" and 32 random bytes.
func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	defer clear(b[:])
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

const cols = `id, user_id, name, url, format, events, enabled, auth_enc IS NOT NULL, created_at, failures, disabled_reason, last_at, last_status`

func scan(r pgx.Row) (Hook, error) {
	var h Hook
	err := r.Scan(&h.ID, &h.UserID, &h.Name, &h.URL, &h.Format, &h.Events, &h.Enabled, &h.HasAuth, &h.CreatedAt, &h.Failures, &h.DisabledReason, &h.LastAt, &h.LastStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

// Create makes a webhook for the user and returns its signing secret, which is never available again.
func (s *Store) Create(ctx context.Context, user uuid.UUID, in Input) (Hook, string, error) {
	if err := s.validate(&in); err != nil {
		return Hook{}, "", err
	}
	sec, err := newSecret()
	if err != nil {
		return Hook{}, "", err
	}
	id := uuid.New()
	encSecret, err := s.Keyring.Seal(loc(id, "secret_enc"), []byte(sec))
	if err != nil {
		return Hook{}, "", err
	}
	var encAuth []byte
	if in.Auth != "" {
		if encAuth, err = s.Keyring.Seal(loc(id, "auth_enc"), []byte(in.Auth)); err != nil {
			return Hook{}, "", err
		}
	}
	var h Hook
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM webhook WHERE user_id = $1", user).Scan(&n); err != nil {
			return err
		}
		if n >= MaxPerUser {
			return ValidationError(fmt.Sprintf("At most %d webhooks: remove one you do not use.", MaxPerUser))
		}
		if h, err = scan(tx.QueryRow(ctx, `INSERT INTO webhook (id, user_id, name, url, format, events, enabled, secret_enc, auth_enc) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+cols, id, user, in.Name, in.URL, in.Format, in.Events, in.Enabled, encSecret, encAuth)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "webhook.create", ObjectType: "webhook", ObjectID: id.String(), Details: map[string]any{"label": in.Name, "events": strings.Join(in.Events, " ")}})
	})
	if err != nil {
		return Hook{}, "", err
	}
	return h, sec, nil
}

// List returns the user's webhooks, by name.
func (s *Store) List(ctx context.Context, user uuid.UUID) ([]Hook, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+cols+" FROM webhook WHERE user_id = $1 ORDER BY lower(name), id", user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Hook, error) { return scan(r) })
}

// Get returns one of the user's webhooks.
func (s *Store) Get(ctx context.Context, user, id uuid.UUID) (Hook, error) {
	return scan(s.Pool.QueryRow(ctx, "SELECT "+cols+" FROM webhook WHERE id = $1 AND user_id = $2", id, user))
}

// Update changes a webhook. Switching it on again clears the count of failures and the reason it was switched off.
func (s *Store) Update(ctx context.Context, user, id uuid.UUID, in Input) (Hook, error) {
	if err := s.validate(&in); err != nil {
		return Hook{}, err
	}
	var h Hook
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		cur, err := scan(tx.QueryRow(ctx, "SELECT "+cols+" FROM webhook WHERE id = $1 AND user_id = $2 FOR UPDATE", id, user))
		if err != nil {
			return err
		}
		mode := 0 // keep the Authorization value
		var encAuth []byte
		switch {
		case in.ClearAuth:
			mode = 1
		case in.Auth != "":
			mode = 2
			if encAuth, err = s.Keyring.Seal(loc(id, "auth_enc"), []byte(in.Auth)); err != nil {
				return err
			}
		}
		failures, reason := cur.Failures, cur.DisabledReason
		if in.Enabled && !cur.Enabled {
			failures, reason = 0, ""
		}
		if h, err = scan(tx.QueryRow(ctx, `UPDATE webhook SET name = $2, url = $3, format = $4, events = $5, enabled = $6, failures = $7, disabled_reason = $8,
				auth_enc = CASE $9::int WHEN 1 THEN NULL WHEN 2 THEN $10::bytea ELSE auth_enc END WHERE id = $1 RETURNING `+cols,
			id, in.Name, in.URL, in.Format, in.Events, in.Enabled, failures, reason, mode, encAuth)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "webhook.update", ObjectType: "webhook", ObjectID: id.String(), Details: map[string]any{"label": in.Name, "enabled": in.Enabled}})
	})
	return h, err
}

// RotateSecret gives the webhook a new signing secret and returns it, once. The old one stops being valid at once.
func (s *Store) RotateSecret(ctx context.Context, user, id uuid.UUID) (string, error) {
	sec, err := newSecret()
	if err != nil {
		return "", err
	}
	enc, err := s.Keyring.Seal(loc(id, "secret_enc"), []byte(sec))
	if err != nil {
		return "", err
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(ctx, "UPDATE webhook SET secret_enc = $3 WHERE id = $1 AND user_id = $2 RETURNING name", id, user, enc).Scan(&name); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "webhook.rotate", ObjectType: "webhook", ObjectID: id.String(), Details: map[string]any{"label": name}})
	})
	if err != nil {
		return "", err
	}
	return sec, nil
}

// Delete removes a webhook and its deliveries.
func (s *Store) Delete(ctx context.Context, user, id uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(ctx, "DELETE FROM webhook WHERE id = $1 AND user_id = $2 RETURNING name", id, user).Scan(&name); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "webhook.delete", ObjectType: "webhook", ObjectID: id.String(), Details: map[string]any{"label": name}})
	})
}

// Delivery is one posting, as the log shows it.
type Delivery struct {
	ID        int64
	UID       uuid.UUID
	Event     string
	State     string
	Attempts  int
	NextAt    time.Time
	CreatedAt time.Time
	DoneAt    *time.Time
	Error     string
	Code      *int
}

// Deliveries returns the newest deliveries of one of the user's webhooks.
func (s *Store) Deliveries(ctx context.Context, user, id uuid.UUID, limit int) ([]Delivery, error) {
	rows, err := s.Pool.Query(ctx, `SELECT d.id, d.uid, d.event, d.state, d.attempts, d.next_at, d.created_at, d.done_at, d.last_error, d.last_code
		FROM webhook_delivery d JOIN webhook w ON w.id = d.webhook_id WHERE d.webhook_id = $1 AND w.user_id = $2 ORDER BY d.id DESC LIMIT $3`, id, user, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) {
		var d Delivery
		err := r.Scan(&d.ID, &d.UID, &d.Event, &d.State, &d.Attempts, &d.NextAt, &d.CreatedAt, &d.DoneAt, &d.Error, &d.Code)
		return d, err
	})
}

// Envelope is the JSON that is posted (format json) or that the message is made from.
type Envelope struct {
	ID        uuid.UUID `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      any       `json:"data"`
}

// Emit queues an event for every enabled webhook of the user that wants it. It returns how many were queued. Emit never blocks on the
// network: the Dispatcher posts.
func (s *Store) Emit(ctx context.Context, user uuid.UUID, event string, data any) (int, error) {
	return s.emit(ctx, user, uuid.Nil, event, data)
}

// Ping queues the test event to one webhook, whether it is enabled and whatever its events.
func (s *Store) Ping(ctx context.Context, user, id uuid.UUID) error {
	h, err := s.Get(ctx, user, id)
	if err != nil {
		return err
	}
	_, err = s.emit(ctx, user, h.ID, "ping", map[string]any{"message": "This is a test from your openDECTcell controller."})
	return err
}

func (s *Store) emit(ctx context.Context, user, only uuid.UUID, event string, data any) (int, error) {
	env := Envelope{ID: uuid.New(), Type: event, CreatedAt: s.now().UTC().Truncate(time.Second), Data: data}
	payload, err := json.Marshal(env)
	if err != nil {
		return 0, err
	}
	q, args := "SELECT id FROM webhook WHERE user_id = $1 AND enabled AND $2 = ANY(events)", []any{user, event}
	if only != uuid.Nil { // the test event goes to the one webhook, enabled or not, whatever it subscribed to
		q, args = "SELECT id FROM webhook WHERE user_id = $1 AND id = $2", []any{user, only}
	}
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO webhook_delivery (webhook_id, uid, event, payload, next_at) VALUES ($1, $2, $3, $4, $5)`, id, env.ID, event, payload, s.now()); err != nil {
			return 0, err
		}
		// a receiver that is gone for a long time must not make the table grow without end: the oldest waiting ones are dropped
		if _, err := s.Pool.Exec(ctx, `DELETE FROM webhook_delivery WHERE webhook_id = $1 AND state = 'pending' AND id <= (
			SELECT id FROM webhook_delivery WHERE webhook_id = $1 AND state = 'pending' ORDER BY id DESC OFFSET $2 LIMIT 1)`, id, MaxPending); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// Prune deletes finished deliveries older than KeepDays.
func (s *Store) Prune(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, "DELETE FROM webhook_delivery WHERE state <> 'pending' AND done_at < $1", now.AddDate(0, 0, -KeepDays))
	return tag.RowsAffected(), err
}
