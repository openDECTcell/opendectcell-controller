// SPDX-License-Identifier: AGPL-3.0-or-later

// Package legal holds the texts an operator can make users read and sign off: a legal notice (Impressum), a privacy notice, an
// emergency-call warning and a notice that the software is experimental. Each is off until an administrator switches it on and
// has free text the administrator writes; the emergency-call and experimental notices also have a standard text. A user must
// have signed off on every enabled text at its current version before they get any access (the web layer enforces it).
package legal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
)

// Kind names one of the four texts.
type Kind string

// The texts, in the order they are shown.
const (
	Imprint      Kind = "imprint"
	Privacy      Kind = "privacy"
	Emergency    Kind = "emergency"
	Experimental Kind = "experimental"
)

// Kinds is every kind, in display order.
var Kinds = []Kind{Imprint, Privacy, Emergency, Experimental}

// MaxText is the longest free text an administrator can store, in bytes.
const MaxText = 20000

// KeySetting is the row of the setting table that holds the texts.
const KeySetting = "legal"

// Title is what the text is called in the menu, on the sign-off page and in the settings.
func (k Kind) Title() string {
	switch k {
	case Imprint:
		return "Legal notice (Impressum)"
	case Privacy:
		return "Privacy notice"
	case Emergency:
		return "Emergency-call warning"
	case Experimental:
		return "Experimental software"
	}
	return string(k)
}

// Short is the label of the sign-off checkbox: what the user declares.
func (k Kind) Short() string {
	switch k {
	case Imprint:
		return "I have read the legal notice."
	case Privacy:
		return "I have read the privacy notice and agree to how my data is handled."
	case Emergency:
		return "I understand that this service cannot be used to make emergency calls."
	case Experimental:
		return "I understand that this software is purely experimental and that there is no guaranteed service."
	}
	return ""
}

// NeedsText reports whether the administrator must write the text: it is specific to the operator, so there is no standard
// one. The other kinds have a standard text and the free text only adds to it.
func (k Kind) NeedsText() bool { return k == Imprint || k == Privacy }

// Standard is the text that comes with the controller for a kind, or "".
func (k Kind) Standard() string {
	switch k {
	case Emergency:
		return "This service must not be relied on for emergency calls. Calls to emergency numbers (112, 110 and the like) may be " +
			"refused, may not reach the right control centre, and cannot be located. If you need to reach the emergency services, " +
			"use a mobile phone or a landline that is not served through this system."
	case Experimental:
		return "This software is experimental. It was written almost entirely (about 99 %) by AI coding agents, and only smaller " +
			"parts of it have been reviewed by a human. It is provided as it is, without any warranty whatsoever, and there is no " +
			"guaranteed service: calls, handsets, base stations and data may stop working, be lost or change at any time, without " +
			"notice. By signing off you confirm that you understand this and use the service at your own risk."
	}
	return ""
}

// Doc is the setting of one text. Version counts up each time the text users have to read changes (or it is switched on again),
// so a changed text is signed off again.
type Doc struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text"`
	Version int    `json:"version"`
}

// Config is the setting: one Doc per kind.
type Config map[Kind]Doc

// Shown is a text as users see it.
type Shown struct {
	Kind    Kind
	Title   string
	Short   string
	Version int
	// Paragraphs are the standard text and then the administrator's, split at blank lines.
	Paragraphs []string
}

// Show builds what users see of an enabled doc.
func Show(k Kind, d Doc) Shown {
	s := Shown{Kind: k, Title: k.Title(), Short: k.Short(), Version: d.Version}
	var raw []string
	if std := k.Standard(); std != "" {
		raw = append(raw, std)
	}
	if t := strings.TrimSpace(d.Text); t != "" {
		raw = append(raw, t)
	}
	for _, block := range raw {
		for _, p := range strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n\n") {
			if p = strings.TrimSpace(p); p != "" {
				s.Paragraphs = append(s.Paragraphs, p)
			}
		}
	}
	return s
}

// Store reads and writes the texts and the sign-offs.
type Store struct {
	Pool     *pgxpool.Pool
	Settings *settings.Store
	Now      func() time.Time // nil: time.Now
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Config returns the setting; a kind never saved is off.
func (s *Store) Config(ctx context.Context) (Config, error) {
	c := Config{}
	if _, err := s.Settings.Get(ctx, KeySetting, &c); err != nil {
		return nil, err
	}
	if c == nil {
		c = Config{}
	}
	return c, nil
}

// Enabled returns the enabled texts as users see them, in display order.
func (s *Store) Enabled(ctx context.Context) ([]Shown, error) {
	c, err := s.Config(ctx)
	if err != nil {
		return nil, err
	}
	return enabled(c), nil
}

func enabled(c Config) []Shown {
	var out []Shown
	for _, k := range Kinds {
		if d := c[k]; d.Enabled && d.Version > 0 {
			out = append(out, Show(k, d))
		}
	}
	return out
}

// ErrInvalid is returned for a setting that cannot be saved; its text says why.
var ErrInvalid = errors.New("legal: invalid setting")

// Input is what the settings form sends for one kind.
type Input struct {
	Enabled bool
	Text    string
}

// Save stores the texts. A text that is switched on (or was off before) or whose words changed gets a new version, so every
// user is asked again; a text left alone keeps its version.
func (s *Store) Save(ctx context.Context, in map[Kind]Input) error {
	old, err := s.Config(ctx)
	if err != nil {
		return err
	}
	next := Config{}
	changed := []string{}
	for _, k := range Kinds {
		i := in[k]
		i.Text = strings.TrimSpace(strings.ReplaceAll(i.Text, "\r\n", "\n"))
		if len(i.Text) > MaxText {
			return fmt.Errorf("%w: the text of the %s is longer than %d characters", ErrInvalid, strings.ToLower(k.Title()), MaxText)
		}
		if i.Enabled && k.NeedsText() && i.Text == "" {
			return fmt.Errorf("%w: write the %s before you switch it on", ErrInvalid, strings.ToLower(k.Title()))
		}
		o := old[k]
		d := Doc{Enabled: i.Enabled, Text: i.Text, Version: o.Version}
		if i.Enabled && (!o.Enabled || o.Text != i.Text || o.Version == 0) {
			d.Version = o.Version + 1
			changed = append(changed, string(k))
		}
		if i.Enabled != o.Enabled && !i.Enabled {
			changed = append(changed, string(k)+" (off)")
		}
		next[k] = d
	}
	if err := s.Settings.Put(ctx, KeySetting, next); err != nil {
		return err
	}
	_ = audit.Record(ctx, s.Pool, audit.Entry{Action: "legal.update", ObjectType: "setting", ObjectID: KeySetting,
		Details: map[string]any{"changed": changed}})
	return nil
}

// Item is a text a user still has to sign off.
type Item = Shown

// Pending returns the enabled texts the user has not signed off at their current version; none means full access.
func (s *Store) Pending(ctx context.Context, user uuid.UUID) ([]Item, error) {
	c, err := s.Config(ctx)
	if err != nil {
		return nil, err
	}
	want := enabled(c)
	if len(want) == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, "SELECT doc, version FROM legal_acceptance WHERE user_id = $1", user)
	if err != nil {
		return nil, err
	}
	have := map[Kind]int{}
	for rows.Next() {
		var k string
		var v int
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return nil, err
		}
		have[Kind(k)] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Item
	for _, w := range want {
		if have[w.Kind] < w.Version {
			out = append(out, w)
		}
	}
	return out, nil
}

// ErrNotAll is returned when the sign-off does not cover every text that is pending, or names a version that is not the current one
// (the administrator changed the text while the user was reading it).
var ErrNotAll = errors.New("legal: every text has to be signed off, in its current version")

// Accept records the user's sign-off. signed maps each ticked kind to the version the user was shown; it must cover every pending
// text at its current version, or nothing is recorded.
func (s *Store) Accept(ctx context.Context, user uuid.UUID, signed map[Kind]int) error {
	pending, err := s.Pending(ctx, user)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if signed[p.Kind] != p.Version {
			return ErrNotAll
		}
	}
	if len(pending) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		docs := []string{}
		for _, p := range pending {
			if _, err := tx.Exec(ctx, `INSERT INTO legal_acceptance (user_id, doc, version, accepted_at) VALUES ($1, $2, $3, $4)
				ON CONFLICT (user_id, doc) DO UPDATE SET version = EXCLUDED.version, accepted_at = EXCLUDED.accepted_at`,
				user, string(p.Kind), p.Version, s.now()); err != nil {
				return err
			}
			docs = append(docs, fmt.Sprintf("%s v%d", p.Kind, p.Version))
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "legal.accept", ObjectType: "user", ObjectID: user.String(),
			Details: map[string]any{"documents": docs}})
	})
}
