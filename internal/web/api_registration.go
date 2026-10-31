// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/regwin"
)

// Registration windows (docs/adr/0005-registration-flow.md): a PIN per
// user's window, registration open on every cell that can take a handset.

func (s *Server) registrationRoutes() {
	if s.Registrations == nil {
		return
	}
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request, *auth.User)) {
		s.mux.Handle(pattern, s.apiUser(fn))
	}
	h("POST "+apiPrefix+"/registration-windows", s.apiWindowOpen)
	h("GET "+apiPrefix+"/registration-windows/{id}", s.apiWindowGet)
	h("DELETE "+apiPrefix+"/registration-windows/{id}", s.apiWindowCancel)
}

func windowJSON(w regwin.Window) map[string]any {
	m := map[string]any{"id": w.ID, "owner_id": w.Owner, "state": w.State,
		"opened_at": w.OpenedAt.UTC().Format(time.RFC3339), "expires_at": w.ExpiresAt.UTC().Format(time.RFC3339),
		"cell_id": nil, "handset_id": nil}
	if w.Cell != uuid.Nil {
		m["cell_id"] = w.Cell
	}
	if w.Handset != uuid.Nil {
		m["handset_id"] = w.Handset
	}
	return m
}

func (s *Server) apiWindowOpen(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var in struct {
		Digits  int        `json:"digits"`
		OwnerID *uuid.UUID `json:"owner_id"`
	}
	if r.ContentLength != 0 {
		if !decode(w, r, &in) {
			return
		}
	}
	owner := u.ID
	if in.OwnerID != nil && *in.OwnerID != u.ID {
		if !u.IsAdmin() {
			problem(w, http.StatusForbidden, "forbidden", "only administrators register for another owner", nil)
			return
		}
		if _, err := s.Auth.UserByID(r.Context(), s.Pool, *in.OwnerID); err != nil {
			s.apiError(w, r, err)
			return
		}
		owner = *in.OwnerID
	}
	win, pin, err := s.Registrations.Open(owner, u.ID, in.Digits)
	switch {
	case errors.Is(err, regwin.ErrDigits):
		problem(w, http.StatusBadRequest, "invalid", "", map[string]string{"digits": "8 or 6"})
		return
	case errors.Is(err, regwin.ErrWindowOpen):
		problem(w, http.StatusConflict, "window_open", "cancel the open window first", nil)
		return
	case errors.Is(err, regwin.ErrRateLimited):
		problem(w, http.StatusTooManyRequests, "rate_limited", "too many registration windows this hour", nil)
		return
	case errors.Is(err, regwin.ErrBusy):
		problem(w, http.StatusServiceUnavailable, "busy", "too many registrations at once; try again in a few minutes", nil)
		return
	case err != nil:
		s.apiError(w, r, err)
		return
	}
	_ = audit.Record(r.Context(), s.Pool, audit.Entry{Action: "registration_window.open",
		ObjectType: "registration_window", ObjectID: win.ID.String(),
		Details: map[string]any{"owner": owner.String()}})
	m := windowJSON(win)
	m["pin"] = pin
	writeJSON(w, http.StatusCreated, m)
}

// window returns the window if u may see it: its opener, its owner, or an
// admin.
func (s *Server) window(w http.ResponseWriter, r *http.Request, u *auth.User) (regwin.Window, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return regwin.Window{}, false
	}
	win, ok := s.Registrations.Get(id)
	if !ok || (!u.IsAdmin() && win.OpenedBy != u.ID && win.Owner != u.ID) {
		notFoundAPI(w)
		return regwin.Window{}, false
	}
	return win, true
}

func (s *Server) apiWindowGet(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if win, ok := s.window(w, r, u); ok {
		writeJSON(w, http.StatusOK, windowJSON(win))
	}
}

func (s *Server) apiWindowCancel(w http.ResponseWriter, r *http.Request, u *auth.User) {
	win, ok := s.window(w, r, u)
	if !ok {
		return
	}
	if _, ok := s.Registrations.Cancel(win.ID); ok && win.State == regwin.Open {
		_ = audit.Record(r.Context(), s.Pool, audit.Entry{Action: "registration_window.cancel",
			ObjectType: "registration_window", ObjectID: win.ID.String()})
	}
	w.WriteHeader(http.StatusNoContent)
}
