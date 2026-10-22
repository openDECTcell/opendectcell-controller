// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
)

func (s *Server) contactRoutes() {
	if s.Contacts == nil {
		return
	}
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request, *auth.User)) {
		s.mux.Handle(pattern, s.apiUser(fn))
	}
	h("GET "+apiPrefix+"/contacts", s.apiContacts)
	h("POST "+apiPrefix+"/contacts", s.apiContactCreate)
	h("GET "+apiPrefix+"/contacts/{id}", s.apiContactGet)
	h("PUT "+apiPrefix+"/contacts/{id}", s.apiContactPut)
	h("DELETE "+apiPrefix+"/contacts/{id}", s.apiContactDelete)
	h("GET "+apiPrefix+"/handsets/{id}/lists/{list}", s.apiHandsetList)
	h("POST "+apiPrefix+"/calls/{id}/read", s.apiCallRead)
}

func (s *Server) contactError(w http.ResponseWriter, r *http.Request, err error) {
	var v contacts.ValidationError
	switch {
	case errors.As(err, &v):
		problem(w, http.StatusBadRequest, "invalid", "", v)
	case errors.Is(err, contacts.ErrNotFound):
		notFoundAPI(w)
	case errors.Is(err, auth.ErrForbidden):
		problem(w, http.StatusForbidden, "forbidden", "", nil)
	case errors.Is(err, contacts.ErrNotMissed):
		problem(w, http.StatusConflict, "not_missed", "only missed calls have a read mark", nil)
	default:
		s.apiError(w, r, err)
	}
}

func int64ID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		notFoundAPI(w)
		return 0, false
	}
	return id, true
}

func nonNilNumbers(c *contacts.Contact) *contacts.Contact {
	if c.Numbers == nil {
		c.Numbers = []contacts.Number{}
	}
	return c
}

func (s *Server) apiContacts(w http.ResponseWriter, r *http.Request, u *auth.User) {
	cs, err := s.Contacts.List(r.Context(), u, r.FormValue("scope"), r.FormValue("owner"))
	if err != nil {
		s.contactError(w, r, err)
		return
	}
	items := make([]*contacts.Contact, len(cs))
	for i, c := range cs {
		items[i] = nonNilNumbers(c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type contactInput struct {
	LastName  string            `json:"last_name"`
	FirstName string            `json:"first_name"`
	Numbers   []contacts.Number `json:"numbers"`
	OwnerID   json.RawMessage   `json:"owner_id"`
}

func (s *Server) apiContactCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var in contactInput
	if !decode(w, r, &in) {
		return
	}
	c := contacts.Contact{LastName: in.LastName, FirstName: in.FirstName, Numbers: in.Numbers}
	setOwner := len(in.OwnerID) > 0
	if setOwner {
		if err := json.Unmarshal(in.OwnerID, &c.OwnerID); err != nil {
			problem(w, http.StatusBadRequest, "invalid", "", map[string]string{"owner_id": "a user id or null"})
			return
		}
	}
	out, err := s.Contacts.Create(r.Context(), u, c, setOwner)
	if err != nil {
		s.contactError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, nonNilNumbers(out))
}

func (s *Server) apiContactGet(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := int64ID(w, r)
	if !ok {
		return
	}
	c, err := s.Contacts.Get(r.Context(), u, id)
	if err != nil {
		s.contactError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNilNumbers(c))
}

func (s *Server) apiContactPut(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := int64ID(w, r)
	if !ok {
		return
	}
	var in contactInput
	if !decode(w, r, &in) {
		return
	}
	if len(in.OwnerID) > 0 {
		problem(w, http.StatusBadRequest, "invalid", "", map[string]string{"owner_id": "set on create only"})
		return
	}
	c, err := s.Contacts.Update(r.Context(), u, id, contacts.Contact{LastName: in.LastName, FirstName: in.FirstName, Numbers: in.Numbers})
	if err != nil {
		s.contactError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNilNumbers(c))
}

func (s *Server) apiContactDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := int64ID(w, r)
	if !ok {
		return
	}
	if err := s.Contacts.Delete(r.Context(), u, id); err != nil {
		s.contactError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiHandsetList returns a list as the handset reads it. The caller must be
// the handset's owner or an admin (shared handsets: admins).
func (s *Server) apiHandsetList(w http.ResponseWriter, r *http.Request, u *auth.User) {
	hid, ok := pathID(w, r)
	if !ok {
		return
	}
	var owner *uuid.UUID
	err := s.Pool.QueryRow(r.Context(), "SELECT owner_user_id FROM handset WHERE id = $1", hid).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !auth.CanAccess(u, owner)) {
		notFoundAPI(w)
		return
	}
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	list := r.PathValue("list")
	if list == "contacts" {
		cs, err := s.Contacts.ForHandset(r.Context(), hid)
		if err != nil {
			s.contactError(w, r, err)
			return
		}
		items := make([]*contacts.Contact, len(cs))
		for i, c := range cs {
			items[i] = nonNilNumbers(c)
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	limit, _ := strconv.Atoi(r.FormValue("limit"))
	es, err := s.Contacts.CallsForHandset(r.Context(), hid, contacts.CallList(list), limit)
	if err != nil {
		var v contacts.ValidationError
		if errors.As(err, &v) {
			notFoundAPI(w) // an unknown list is an unknown resource
			return
		}
		s.contactError(w, r, err)
		return
	}
	if es == nil {
		es = []contacts.CallEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": es})
}

func (s *Server) apiCallRead(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := int64ID(w, r)
	if !ok {
		return
	}
	if err := s.Contacts.MarkMissedRead(r.Context(), u, id); err != nil {
		s.contactError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
