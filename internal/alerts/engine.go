// SPDX-License-Identifier: AGPL-3.0-or-later

package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Who is told: these users, and the administrators if Admins is set.
type Who struct {
	Users  []uuid.UUID `json:"users,omitempty"`
	Admins bool        `json:"admins,omitempty"`
}

// Notification is what one person is told. Level is ok (it is over), info, warning or danger.
type Notification struct {
	Kind, Level, Subject, Body, Href string
}

// Status is what a source says of a watched object.
type Status int

const (
	// Wrong says it is wrong now.
	Wrong Status = iota
	// Fine says it was looked at and is fine. Only this ends an episode that was told.
	Fine
	// Unsure says it is in between (a line that is registering again): the episode, and its clock, stay as they are.
	Unsure
)

// Condition is a watched object: something that is wrong now, or fine, or in between. A wrong one is told once it has been wrong for
// After, counted from Since (the zero time: from when it was first seen), and its end is told once if the start was. An object the
// source does not mention at all (it was deleted, switched off, is not watched any more) is forgotten without a word: that it is gone
// does not mean that it got better.
type Condition struct {
	Status Status
	Kind   string
	Object string // what it is about, with Kind the identity of the episode
	Who    Who
	Since  time.Time
	After  time.Duration
	Level  string
	Title  string
	Body   string
	Href   string
	// OkTitle and OkBody are said when it is over.
	OkTitle, OkBody string
}

// Source finds the conditions of its Kinds that are wrong at the moment. If it fails, nothing is concluded about those kinds: absence
// of a condition is only news when the source could look.
type Source struct {
	Kinds []string
	Find  func(ctx context.Context, now time.Time) ([]Condition, error)
}

// Engine looks at its sources once a minute and tells people. Its state is in the database (alert_state), so a restart repeats nothing.
type Engine struct {
	Pool    *pgxpool.Pool
	Sources []Source
	Now     func() time.Time
	Log     *slog.Logger
	// Sink, if set, is told every notification that was made (the webhooks of ideas F2).
	Sink func(user uuid.UUID, n Notification)
}

// Run ticks until ctx ends.
func (e *Engine) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
				e.log().Warn("alerts", "err", err)
			}
		}
	}
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

type stateInfo struct {
	Who     Who    `json:"who"`
	Level   string `json:"level"`
	Href    string `json:"href,omitempty"`
	OkTitle string `json:"ok_title,omitempty"`
	OkBody  string `json:"ok_body,omitempty"`
}

// Tick is one look at every source.
func (e *Engine) Tick(ctx context.Context) error {
	now := e.now()
	var errs []error
	for _, src := range e.Sources {
		conds, err := src.Find(ctx, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		status := map[string]Status{}
		for _, c := range conds {
			if !slices.Contains(src.Kinds, c.Kind) {
				continue // a source speaks for its own kinds only
			}
			status[c.Kind+"\x00"+c.Object] = c.Status
			if c.Status != Wrong {
				continue
			}
			if err := e.seen(ctx, now, c); err != nil {
				errs = append(errs, err)
			}
		}
		if err := e.recover(ctx, src.Kinds, status); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// seen records that c is wrong now and tells it if it has been for long enough and has not been told.
func (e *Engine) seen(ctx context.Context, now time.Time, c Condition) error {
	since := c.Since
	if since.IsZero() {
		since = now
	}
	info, _ := json.Marshal(stateInfo{Who: c.Who, Level: c.Level, Href: c.Href, OkTitle: c.OkTitle, OkBody: c.OkBody})
	var fired *time.Time
	var first time.Time
	err := e.Pool.QueryRow(ctx, `INSERT INTO alert_state (kind, object, since, info) VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, object) DO UPDATE SET info = EXCLUDED.info RETURNING since, fired_at`, c.Kind, c.Object, since, info).Scan(&first, &fired)
	if err != nil {
		return err
	}
	if fired != nil || now.Sub(first) < c.After {
		return nil
	}
	// tell first, then record it: a crash in between tells twice rather than never
	if err := e.Notify(ctx, c.Who, Notification{Kind: c.Kind, Level: c.Level, Subject: c.Title, Body: c.Body, Href: c.Href}); err != nil {
		return err
	}
	_, err = e.Pool.Exec(ctx, "UPDATE alert_state SET fired_at = $3 WHERE kind = $1 AND object = $2", c.Kind, c.Object, now)
	return err
}

// recover tells the end of the episodes of kinds whose object is fine now, if they were told, and forgets those that are not watched any more.
func (e *Engine) recover(ctx context.Context, kinds []string, status map[string]Status) error {
	rows, err := e.Pool.Query(ctx, "SELECT kind, object, fired_at IS NOT NULL, info FROM alert_state WHERE kind = ANY($1)", kinds)
	if err != nil {
		return err
	}
	type gone struct {
		kind, object string
		told, fine   bool
		info         stateInfo
	}
	var all []gone
	for rows.Next() {
		var g gone
		var raw []byte
		if err := rows.Scan(&g.kind, &g.object, &g.told, &raw); err != nil {
			rows.Close()
			return err
		}
		_ = json.Unmarshal(raw, &g.info)
		if st, watched := status[g.kind+"\x00"+g.object]; !watched || st == Fine {
			g.fine = watched
			all = append(all, g)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, g := range all {
		if g.fine && g.told && g.info.OkTitle != "" {
			if err := e.Notify(ctx, g.info.Who, Notification{Kind: g.kind, Level: "ok", Subject: g.info.OkTitle, Body: g.info.OkBody, Href: g.info.Href}); err != nil {
				errs = append(errs, err)
				continue // keep the state: tell it next time
			}
		}
		if _, err := e.Pool.Exec(ctx, "DELETE FROM alert_state WHERE kind = $1 AND object = $2", g.kind, g.object); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Notify makes the notification for each person in who whose preferences show its kind or ask for mail of it.
func (e *Engine) Notify(ctx context.Context, who Who, n Notification) error {
	return Notify(ctx, e.Pool, who, n, e.Sink)
}

// Notify is Engine.Notify without an engine, for what is told at the moment it happens (a base station that goes offline).
func Notify(ctx context.Context, pool *pgxpool.Pool, who Who, n Notification, sink func(uuid.UUID, Notification)) error {
	ids := slices.Clone(who.Users)
	if who.Admins {
		rows, err := pool.Query(ctx, "SELECT id FROM app_user WHERE role = 'admin' AND NOT disabled")
		if err != nil {
			return err
		}
		admins, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		ids = append(ids, admins...)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	ids = slices.Compact(ids)
	var errs []error
	for _, id := range ids {
		var raw []byte
		if err := pool.QueryRow(ctx, "SELECT alert_prefs FROM app_user WHERE id = $1 AND NOT disabled", id).Scan(&raw); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				errs = append(errs, err)
			}
			continue
		}
		p := ParsePrefs(raw)
		show, mail := p.Shows(n.Kind), p.Mails(n.Kind)
		if !show && !mail {
			continue
		}
		var readAt *time.Time
		if !show { // wanted by mail only: it is on the page, read
			now := time.Now()
			readAt = &now
		}
		if _, err := pool.Exec(ctx, `INSERT INTO notification (user_id, kind, level, subject, body, href, read_at, mail_wanted)
			VALUES ($1, $2, $3, left($4, 200), left($5, 2000), left($6, 200), $7, $8)`, id, n.Kind, n.Level, n.Subject, n.Body, n.Href, readAt, mail); err != nil {
			errs = append(errs, err)
			continue
		}
		if sink != nil {
			sink(id, n)
		}
	}
	return errors.Join(errs...)
}
