// SPDX-License-Identifier: AGPL-3.0-or-later

// Package settings holds the installation-wide settings an admin changes in
// the UI: the instance's name and logo, self-registration, logging. They live
// in the setting table as JSON (one row per group) and the logo in asset.
package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Keys of the setting table.
const (
	KeyInstance     = "instance"
	KeyRegistration = "registration"
	KeyLogging      = "logging"
	KeyMap          = "map"
)

// MaxLogoBytes is the largest logo the controller stores.
const MaxLogoBytes = 256 << 10

// Store reads and writes the settings.
type Store struct {
	Pool *pgxpool.Pool
}

// Instance names this controller (the sidebar, the login page, e-mails).
type Instance struct {
	Name    string `json:"name"`
	Tagline string `json:"tagline"`
	HasLogo bool   `json:"has_logo"`
}

// DefaultName is shown until an admin names the controller.
const DefaultName = "openDECTcell controller"

// Registration is the self-registration policy. Off until an admin turns it
// on: an installation that never opened it has no sign-up page.
type Registration struct {
	Enabled         bool     `json:"enabled"`
	RequireApproval bool     `json:"require_approval"`
	AllowedDomains  []string `json:"allowed_domains"` // e-mail domains; empty: any
}

// Get reads the setting key into v. It reports false if the row is absent,
// leaving v as it was.
func (s *Store) Get(ctx context.Context, key string, v any) (bool, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx, "SELECT value FROM setting WHERE key = $1", key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// Put writes the setting key.
func (s *Store) Put(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO setting (key, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, raw)
	return err
}

// Instance returns the instance settings, with the default name filled in.
func (s *Store) Instance(ctx context.Context) (Instance, error) {
	var i Instance
	if _, err := s.Get(ctx, KeyInstance, &i); err != nil {
		return i, err
	}
	if i.Name == "" {
		i.Name = DefaultName
	}
	return i, nil
}

// Registration returns the self-registration policy.
func (s *Store) Registration(ctx context.Context) (Registration, error) {
	var r Registration
	_, err := s.Get(ctx, KeyRegistration, &r)
	return r, err
}

// AllowsEmail reports whether an address is acceptable under the domain list.
func (r Registration) AllowsEmail(email string) bool {
	if len(r.AllowedDomains) == 0 {
		return true
	}
	_, dom, ok := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if !ok {
		return false
	}
	for _, d := range r.AllowedDomains {
		if strings.EqualFold(strings.TrimSpace(d), dom) {
			return true
		}
	}
	return false
}

// ErrBadLogo is returned for a file that is not an acceptable logo.
var ErrBadLogo = errors.New("settings: the logo must be a PNG, JPEG or WebP image of at most 256 KiB")

// LogoType sniffs data and returns its content type, or ErrBadLogo. SVG is
// refused on purpose: it can carry script and styles, and the controller
// serves the logo from its own origin.
func LogoType(data []byte) (string, error) {
	if len(data) == 0 || len(data) > MaxLogoBytes {
		return "", ErrBadLogo
	}
	ct := http.DetectContentType(data)
	switch {
	case ct == "image/png", ct == "image/jpeg", ct == "image/webp":
		return ct, nil
	case bytes.HasPrefix(data, []byte("RIFF")) && len(data) > 12 && string(data[8:12]) == "WEBP":
		return "image/webp", nil
	}
	return "", ErrBadLogo
}

// SetLogo stores the logo and marks the instance as having one.
func (s *Store) SetLogo(ctx context.Context, data []byte) error {
	ct, err := LogoType(data)
	if err != nil {
		return err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // after Commit it is a no-op
	if _, err := tx.Exec(ctx, `INSERT INTO asset (key, content_type, data, updated_at) VALUES ('logo', $1, $2, now())
		ON CONFLICT (key) DO UPDATE SET content_type = EXCLUDED.content_type, data = EXCLUDED.data, updated_at = now()`, ct, data); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO setting (key, value, updated_at)
		VALUES ($1, jsonb_build_object('has_logo', true), now())
		ON CONFLICT (key) DO UPDATE SET value = setting.value || jsonb_build_object('has_logo', true), updated_at = now()`, KeyInstance); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ClearLogo removes the logo.
func (s *Store) ClearLogo(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // after Commit it is a no-op
	if _, err := tx.Exec(ctx, "DELETE FROM asset WHERE key = 'logo'"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE setting SET value = value || jsonb_build_object('has_logo', false), updated_at = now() WHERE key = $1`, KeyInstance); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Logo returns the stored logo, if any.
func (s *Store) Logo(ctx context.Context) (contentType string, data []byte, updated time.Time, ok bool, err error) {
	err = s.Pool.QueryRow(ctx, "SELECT content_type, data, updated_at FROM asset WHERE key = 'logo'").Scan(&contentType, &data, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, time.Time{}, false, nil
	}
	return contentType, data, updated, err == nil, err
}

// Map says where the position pickers and the public map get their tiles.
// Empty means none: no request leaves for a tile server, and the pickers
// work with typed coordinates. A tile server learns each visitor's address,
// which is why the admin chooses one on purpose.
type Map struct {
	TileURL     string `json:"tile_url"`    // https://host/{z}/{x}/{y}.png
	Attribution string `json:"attribution"` // shown on the map
}

// OSMTileURL is the OpenStreetMap Foundation's standard tile server. Its usage
// policy is for light use; the choice and its privacy cost are the admin's.
const OSMTileURL = "https://tile.openstreetmap.org/{z}/{x}/{y}.png"

// OSMAttribution is what OSM asks to be shown.
const OSMAttribution = "© OpenStreetMap contributors"

// Map returns the tile settings.
func (s *Store) Map(ctx context.Context) (Map, error) {
	var m Map
	_, err := s.Get(ctx, KeyMap, &m)
	return m, err
}

// ErrBadTileURL is returned by Validate for a tile URL the controller will not use.
var ErrBadTileURL = errors.New("the tile URL must be https and contain {z}, {x} and {y}")

// Validate checks the tile URL and returns its origin for the page's
// Content-Security-Policy ("" for no tiles).
func (m Map) Validate() (origin string, err error) {
	if m.TileURL == "" {
		return "", nil
	}
	u, perr := url.Parse(strings.NewReplacer("{z}", "0", "{x}", "0", "{y}", "0").Replace(m.TileURL))
	if perr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		!strings.Contains(m.TileURL, "{z}") || !strings.Contains(m.TileURL, "{x}") || !strings.Contains(m.TileURL, "{y}") ||
		strings.ContainsAny(m.TileURL, " \t\r\n\"'<>;,") || len(m.Attribution) > 200 {
		return "", ErrBadTileURL
	}
	if strings.Contains(u.Host, "{") {
		return "", ErrBadTileURL
	}
	return u.Scheme + "://" + u.Host, nil
}
