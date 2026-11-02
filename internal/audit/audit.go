// SPDX-License-Identifier: AGPL-3.0-or-later

// Package audit records state-changing actions in the append-only
// audit_log table (docs/schema.md). Write the entry in the same transaction
// as the change it describes, so neither exists without the other.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/db"
)

// Actor is who performed an action.
type Actor struct {
	UserID *uuid.UUID // nil for the system or a cell
	Name   string     // snapshot: username, "system", "cell:<id>", "cli"
	Addr   netip.Addr // client address, if known
}

// System is the actor for actions the controller takes on its own.
var System = Actor{Name: "system"}

// Entry is one audit record. Details must never contain secrets: keys whose
// names suggest a secret are refused (see forbiddenKey).
type Entry struct {
	Action     string // dotted verb, e.g. "user.login", "handset.delete"
	ObjectType string
	ObjectID   string
	Details    map[string]any
}

type actorKey struct{}

// WithActor returns a context carrying the actor for Record.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor in ctx, or System.
func ActorFrom(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorKey{}).(Actor); ok {
		return a
	}
	return System
}

var secretWords = []string{"password", "passwd", "secret", "token", "pin", "key", "uak", "dck", "credential", "cookie", "nonce", "verifier"}

// forbiddenKey reports whether a details key looks like it names a secret:
// one of its words (split at _, -, . and lower-to-upper case changes) is a
// secret word. Names ending in an id or count word ("key_id",
// "token_count") identify or count secrets and are allowed.
func forbiddenKey(k string) bool {
	words := splitWords(k)
	if n := len(words); n > 1 && (words[n-1] == "id" || words[n-1] == "count" || words[n-1] == "fingerprint") {
		return false
	}
	for _, w := range words {
		for _, s := range secretWords {
			if w == s || w == s+"s" {
				return true
			}
		}
	}
	return false
}

func splitWords(k string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	var prev rune
	for _, r := range k {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ':
			flush()
		case r >= 'A' && r <= 'Z' && prev >= 'a' && prev <= 'z':
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	return words
}

func checkDetails(m map[string]any) error {
	for k, v := range m {
		if forbiddenKey(k) {
			return fmt.Errorf("audit: details key %q looks like a secret; record that it changed, not its value", k)
		}
		if sub, ok := v.(map[string]any); ok {
			if err := checkDetails(sub); err != nil {
				return err
			}
		}
	}
	return nil
}

// Record writes an entry for the actor in ctx.
func Record(ctx context.Context, q db.Querier, e Entry) error {
	return RecordAs(ctx, q, ActorFrom(ctx), e)
}

// RecordAs writes an entry for an explicit actor.
func RecordAs(ctx context.Context, q db.Querier, a Actor, e Entry) error {
	if e.Action == "" || a.Name == "" {
		return errors.New("audit: action and actor name are required")
	}
	if err := checkDetails(e.Details); err != nil {
		return err
	}
	details := []byte("{}")
	if len(e.Details) > 0 {
		var err error
		if details, err = json.Marshal(e.Details); err != nil {
			return fmt.Errorf("audit: %w", err)
		}
	}
	var addr *string
	if a.Addr.IsValid() {
		s := a.Addr.Unmap().String()
		addr = &s
	}
	_, err := q.Exec(ctx, `INSERT INTO audit_log (actor_user_id, actor_name, action, object_type, object_id, details, remote_addr)
		VALUES ($1, $2, $3, $4, $5, $6, $7::inet)`,
		a.UserID, a.Name, e.Action, e.ObjectType, e.ObjectID, details, addr)
	return err
}

// Row is an audit entry as read back.
type Row struct {
	ID         int64
	At         time.Time
	ActorID    *uuid.UUID
	ActorName  string
	Action     string
	ObjectType string
	ObjectID   string
	Details    map[string]any
	RemoteAddr *netip.Addr
}

// Filter selects audit entries. Zero fields match everything.
type Filter struct {
	ActorID    *uuid.UUID
	ObjectType string
	ObjectID   string
	Before     int64 // entries with id < Before (paging); 0 = newest
	Limit      int   // default 50, max 500
}

// List returns entries newest first.
func List(ctx context.Context, q db.Querier, f Filter) ([]Row, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	rows, err := q.Query(ctx, `SELECT id, at, actor_user_id, actor_name, action, object_type, object_id, details, remote_addr
		FROM audit_log
		WHERE ($1::uuid IS NULL OR actor_user_id = $1)
		  AND ($2 = '' OR object_type = $2)
		  AND ($3 = '' OR object_id = $3)
		  AND ($4 = 0 OR id < $4)
		ORDER BY id DESC LIMIT $5`, f.ActorID, f.ObjectType, f.ObjectID, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var details []byte
		if err := rows.Scan(&r.ID, &r.At, &r.ActorID, &r.ActorName, &r.Action, &r.ObjectType, &r.ObjectID, &details, &r.RemoteAddr); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(details, &r.Details); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
