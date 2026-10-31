// SPDX-License-Identifier: AGPL-3.0-or-later

// Package logctl lets an admin change how much the controller logs while it
// runs, per component, from nothing at all up to every call (debug). The
// loggers are the ones main already derives with .With("component", name); a
// logger without a component belongs to "core".
//
// A level can carry an expiry: debug and calls levels are for chasing a
// problem and fall back to the default by themselves, so a forgotten debug
// switch does not fill a disk or keep personal data in the log.
package logctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Levels, from quietest to loudest. LevelCalls is below slog's debug: it is
// for lines about individual calls (who, which cell, codec, quality).
const (
	LevelOff   slog.Level = 100
	LevelCalls slog.Level = slog.LevelDebug - 4
)

// Names of the levels as the UI and the settings store them.
var Names = []string{"off", "error", "warn", "info", "debug", "calls"}

// Components are the parts that can be set separately.
var Components = []string{"core", "web", "auth", "cells", "dect", "sip", "media"}

// ParseLevel turns a level name into a slog level.
func ParseLevel(s string) (slog.Level, error) {
	switch s {
	case "off":
		return LevelOff, nil
	case "error":
		return slog.LevelError, nil
	case "warn":
		return slog.LevelWarn, nil
	case "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "calls":
		return LevelCalls, nil
	}
	return 0, fmt.Errorf("logctl: unknown level %q (want one of %v)", s, Names)
}

// Name is the level's name, or "" for a level between the named ones.
func Name(l slog.Level) string {
	for _, n := range Names {
		if v, _ := ParseLevel(n); v == l {
			return n
		}
	}
	return ""
}

// Setting is the state of one component.
type Setting struct {
	Level string     `json:"level"`
	Until *time.Time `json:"until,omitempty"` // after this the default applies again
}

// State is what is persisted and shown.
type State struct {
	Default    string             `json:"default"`
	Components map[string]Setting `json:"components,omitempty"`
}

// Controller holds the live levels.
type Controller struct {
	mu   sync.Mutex
	def  slog.Level
	vars map[string]*slog.LevelVar
	comp map[string]Setting
	now  func() time.Time
}

// New returns a controller whose default level is def.
func New(def slog.Level) *Controller {
	c := &Controller{def: def, vars: map[string]*slog.LevelVar{}, comp: map[string]Setting{}, now: time.Now}
	for _, n := range Components {
		v := new(slog.LevelVar)
		v.Set(def)
		c.vars[n] = v
	}
	return c
}

// Handler wraps inner so that its records are filtered by the component's
// level. inner itself must let everything through down to LevelCalls.
func (c *Controller) Handler(inner slog.Handler) slog.Handler {
	return &handler{c: c, inner: inner, lv: c.vars["core"]}
}

// Apply sets the state, for the settings loaded at start. Unknown components
// and levels are ignored; an expired entry counts as the default.
func (c *Controller) Apply(st State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, err := ParseLevel(st.Default); err == nil {
		c.def = d
	}
	c.comp = map[string]Setting{}
	for _, n := range Components {
		c.vars[n].Set(c.def)
	}
	for n, s := range st.Components {
		v, ok := c.vars[n]
		l, err := ParseLevel(s.Level)
		if !ok || err != nil || (s.Until != nil && !s.Until.After(c.now())) {
			continue
		}
		v.Set(l)
		c.comp[n] = s
	}
}

// ErrUnknownComponent is returned by Set for a name not in Components.
var ErrUnknownComponent = errors.New("logctl: unknown component")

// Set changes one component ("" sets the default for all components without
// an override). A zero ttl lasts until changed again.
func (c *Controller) Set(component, level string, ttl time.Duration) error {
	l, err := ParseLevel(level)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if component == "" {
		c.def = l
		for n, v := range c.vars {
			if _, over := c.comp[n]; !over {
				v.Set(l)
			}
		}
		return nil
	}
	v, ok := c.vars[component]
	if !ok {
		return ErrUnknownComponent
	}
	v.Set(l)
	s := Setting{Level: level}
	if ttl > 0 {
		u := c.now().Add(ttl)
		s.Until = &u
	}
	c.comp[component] = s
	return nil
}

// Clear removes a component's override, so the default applies to it again.
func (c *Controller) Clear(component string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.vars[component]; ok {
		v.Set(c.def)
		delete(c.comp, component)
	}
}

// State returns the current state.
func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := State{Default: Name(c.def), Components: map[string]Setting{}}
	for n, s := range c.comp {
		st.Components[n] = s
	}
	return st
}

// Effective returns every component's level name, in Components order.
func (c *Controller) Effective() []Row {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows := make([]Row, 0, len(Components))
	for _, n := range Components {
		r := Row{Component: n, Level: Name(c.vars[n].Level())}
		if s, ok := c.comp[n]; ok {
			r.Until = s.Until
			r.Override = true
		}
		rows = append(rows, r)
	}
	return rows
}

// Row is one component's effective level.
type Row struct {
	Component string
	Level     string
	Override  bool
	Until     *time.Time
}

// Expire resets every override whose time has come and returns their
// components, so the caller can persist and announce the change.
func (c *Controller) Expire() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for n, s := range c.comp {
		if s.Until != nil && !s.Until.After(c.now()) {
			c.vars[n].Set(c.def)
			delete(c.comp, n)
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

type handler struct {
	c     *Controller
	inner slog.Handler
	lv    *slog.LevelVar
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.lv.Level() }

func (h *handler) Handle(ctx context.Context, r slog.Record) error { return h.inner.Handle(ctx, r) }

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	n := &handler{c: h.c, inner: h.inner.WithAttrs(as), lv: h.lv}
	for _, a := range as {
		if a.Key == "component" {
			if v, ok := h.c.vars[a.Value.String()]; ok {
				n.lv = v
			}
		}
	}
	return n
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{c: h.c, inner: h.inner.WithGroup(name), lv: h.lv}
}
