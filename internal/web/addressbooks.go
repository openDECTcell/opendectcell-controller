// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/carddav"
)

func (s *Server) addressBookRoutes() {
	if s.Books == nil || s.Contacts == nil {
		return
	}
	s.mux.Handle("POST /address-books", s.requireUser(s.bookAdd))
	s.mux.Handle("GET /address-books/{id}", s.requireUser(s.bookPage))
	s.mux.Handle("POST /address-books/{id}", s.requireUser(s.bookSave))
	s.mux.Handle("POST /address-books/{id}/sync", s.requireUser(s.bookSync))
	s.mux.Handle("POST /address-books/{id}/delete", s.requireUser(s.bookDelete))
}

// bookSyncTimeout is how long a sync that a user asked for may take: they wait for it.
const bookSyncTimeout = 40 * time.Second

func bookInput(r *http.Request) carddav.Input {
	iv, _ := strconv.Atoi(r.PostFormValue("interval"))
	return carddav.Input{Label: r.PostFormValue("label"), URL: r.PostFormValue("url"), Username: r.PostFormValue("username"),
		Password: r.PostFormValue("password"), Enabled: r.PostFormValue("enabled") == "on", IntervalMin: iv}
}

// syncNow reads a book while the user waits and says in a sentence how it went.
func (s *Server) syncNow(ctx context.Context, u *auth.User, id uuid.UUID) (ok bool, msg string) {
	cctx, cancel := context.WithTimeout(ctx, bookSyncTimeout)
	defer cancel()
	if err := s.Books.Sync(cctx, id); err != nil {
		return false, err.Error()
	}
	b, err := s.Books.Get(ctx, u, id)
	if err != nil {
		return true, "Synchronised."
	}
	return true, "Synchronised: " + strconv.Itoa(b.Count) + " contacts."
}

func (s *Server) bookAdd(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	in := bookInput(r)
	in.Enabled = true
	var attach []uuid.UUID
	if r.PostFormValue("all_handsets") == "on" && s.Handsets != nil {
		if hs, err := s.Handsets.List(r.Context(), u); err == nil {
			for _, h := range hs {
				if h.OwnerID == u.ID {
					attach = append(attach, h.ID)
				}
			}
		}
	}
	b, err := s.Books.Create(r.Context(), u, in, attach)
	var v carddav.ValidationError
	if errors.As(err, &v) {
		s.renderContacts(w, r, u, http.StatusBadRequest, contactsData{BookErrors: v, BookForm: &in}, page{Error: "Please correct the address book."})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	ok, msg := s.syncNow(r.Context(), u, b.ID)
	if !ok {
		msg = "The address book is linked, but it could not be read: " + msg + " Open it to change the address or the password."
	}
	http.Redirect(w, r, "/contacts?done="+url.QueryEscape(msg), http.StatusSeeOther)
}

type bookView struct {
	Book    *carddav.Book
	Errors  carddav.ValidationError
	Form    *carddav.Input
	Handset int // handsets that show it
}

func (s *Server) bookByPath(w http.ResponseWriter, r *http.Request, u *auth.User) (*carddav.Book, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, u)
		return nil, false
	}
	b, err := s.Books.Get(r.Context(), u, id)
	if errors.Is(err, carddav.ErrNotFound) {
		s.notFound(w, r, u)
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return nil, false
	}
	return b, true
}

func (s *Server) bookPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	b, ok := s.bookByPath(w, r, u)
	if !ok {
		return
	}
	s.render(w, http.StatusOK, "address_book.html", page{Title: b.Label, User: u, Data: bookView{Book: b}, Notice: r.FormValue("done")})
}

func (s *Server) bookSave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	b, ok := s.bookByPath(w, r, u)
	if !ok {
		return
	}
	in := bookInput(r)
	_, err := s.Books.Update(r.Context(), u, b.ID, in)
	var v carddav.ValidationError
	if errors.As(err, &v) {
		in.Password = ""
		s.render(w, http.StatusBadRequest, "address_book.html", page{Title: b.Label, User: u, Data: bookView{Book: b, Errors: v, Form: &in}, Error: "Please correct the address book."})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	ok, msg := s.syncNow(r.Context(), u, b.ID)
	if !ok {
		msg = "Saved, but the address book could not be read: " + msg
	}
	http.Redirect(w, r, "/address-books/"+b.ID.String()+"?done="+url.QueryEscape(msg), http.StatusSeeOther) //nolint:gosec // G710: our UUID
}

func (s *Server) bookSync(w http.ResponseWriter, r *http.Request, u *auth.User) {
	b, ok := s.bookByPath(w, r, u)
	if !ok {
		return
	}
	_, msg := s.syncNow(r.Context(), u, b.ID)
	http.Redirect(w, r, "/address-books/"+b.ID.String()+"?done="+url.QueryEscape(msg), http.StatusSeeOther) //nolint:gosec // G710: our UUID
}

func (s *Server) bookDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	b, ok := s.bookByPath(w, r, u)
	if !ok {
		return
	}
	if err := s.Books.Delete(r.Context(), u, b.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/contacts?done=Address+book+removed.", http.StatusSeeOther)
}
