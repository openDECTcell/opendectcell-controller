// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
)

func (s *Server) contactPages() {
	if s.Contacts == nil {
		return
	}
	s.mux.Handle("GET /contacts", s.requireUser(s.contactsPage))
	s.mux.Handle("POST /contacts", s.requireUser(s.contactAdd))
	s.mux.Handle("POST /contacts/{id}/delete", s.requireUser(s.contactRemove))
	s.contactExchangeRoutes()
}

type contactsData struct {
	Personal, Shared []*contacts.Contact
	Errors           contacts.ValidationError
	Form             contacts.Contact
	SharedChecked    bool
	Import           *importResult
}

func (s *Server) renderContacts(w http.ResponseWriter, r *http.Request, u *auth.User, status int, d contactsData, p page) {
	all, err := s.Contacts.List(r.Context(), u, "", "")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for _, c := range all {
		if c.OwnerID == nil {
			d.Shared = append(d.Shared, c)
		} else {
			d.Personal = append(d.Personal, c)
		}
	}
	p.Title, p.User, p.Data = "Contacts", u, d
	s.render(w, status, "contacts.html", p)
}

func (s *Server) contactsPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.renderContacts(w, r, u, http.StatusOK, contactsData{}, page{Notice: r.FormValue("done")})
}

func (s *Server) contactAdd(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	c := contacts.Contact{LastName: r.PostFormValue("last_name"), FirstName: r.PostFormValue("first_name")}
	for i := range contacts.MaxNumbers {
		n := strings.TrimSpace(r.PostFormValue("number" + strconv.Itoa(i)))
		if n != "" {
			c.Numbers = append(c.Numbers, contacts.Number{Number: n, Kind: r.PostFormValue("kind" + strconv.Itoa(i))})
		}
	}
	shared := r.PostFormValue("shared") == "on"
	_, err := s.Contacts.Create(r.Context(), u, c, shared)
	var v contacts.ValidationError
	switch {
	case errors.As(err, &v):
		s.renderContacts(w, r, u, http.StatusBadRequest, contactsData{Errors: v, Form: c, SharedChecked: shared}, page{Error: "Please correct the contact."})
	case errors.Is(err, auth.ErrForbidden):
		s.renderContacts(w, r, u, http.StatusForbidden, contactsData{Form: c}, page{Error: "Only administrators can add shared contacts."})
	case err != nil:
		s.internalError(w, r, err)
	default:
		http.Redirect(w, r, "/contacts?done=Added.", http.StatusSeeOther)
	}
}

func (s *Server) contactRemove(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, u)
		return
	}
	switch err := s.Contacts.Delete(r.Context(), u, id); {
	case errors.Is(err, contacts.ErrNotFound):
		s.notFound(w, r, u)
	case errors.Is(err, auth.ErrForbidden):
		s.renderContacts(w, r, u, http.StatusForbidden, contactsData{}, page{Error: "Only administrators can delete shared contacts."})
	case err != nil:
		s.internalError(w, r, err)
	default:
		http.Redirect(w, r, "/contacts?done=Deleted.", http.StatusSeeOther)
	}
}
