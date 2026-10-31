// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
)

func (s *Server) handsetRoutes() {
	if s.Handsets == nil {
		return
	}
	s.mux.Handle("GET /handsets", s.requireUser(s.handsetList))
	s.mux.Handle("GET /handsets/register", s.requireUser(s.handsetRegister))
	s.mux.Handle("GET /handsets/{id}", s.requireUser(s.handsetPage))
	s.mux.Handle("POST /handsets/{id}", s.requireUser(s.handsetSave))
	s.mux.Handle("POST /handsets/{id}/lines/{line}", s.requireUser(s.handsetLine))
}

func (s *Server) handsetList(w http.ResponseWriter, r *http.Request, u *auth.User) {
	hs, err := s.Handsets.List(r.Context(), u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "handsets.html", page{Title: "Handsets", User: u, Data: hs, Notice: r.FormValue("done")})
}

func (s *Server) handsetRegister(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.render(w, http.StatusOK, "handset_register.html", page{Title: "Register a handset", User: u, Data: s.Registrations != nil})
}

type handsetPage struct {
	H        *handset.Handset
	Accounts []handset.Account
	Line     map[int]*handset.Line
	Moves    []handset.Move
}

func (s *Server) handsetView(r *http.Request, h *handset.Handset) (handsetPage, error) {
	accts, err := s.Handsets.Accounts(r.Context(), h.OwnerID)
	d := handsetPage{H: h, Accounts: accts, Line: map[int]*handset.Line{}}
	if err == nil {
		d.Moves, err = s.Handsets.Moves(r.Context(), h.ID, 10)
	}
	for i := range h.Lines {
		d.Line[h.Lines[i].Line] = &h.Lines[i]
	}
	return d, err
}

func (s *Server) handsetByPath(w http.ResponseWriter, r *http.Request, u *auth.User) (*handset.Handset, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, u)
		return nil, false
	}
	h, err := s.Handsets.Get(r.Context(), u, id)
	if errors.Is(err, handset.ErrNotFound) {
		s.notFound(w, r, u)
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return nil, false
	}
	return h, true
}

func (s *Server) handsetPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	h, ok := s.handsetByPath(w, r, u)
	if !ok {
		return
	}
	d, err := s.handsetView(r, h)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "handset.html", page{Title: h.Display(), User: u, Data: d, Notice: r.FormValue("done")})
}

func (s *Server) handsetFail(w http.ResponseWriter, r *http.Request, u *auth.User, h *handset.Handset, err error) {
	if errors.Is(err, handset.ErrNotFound) {
		s.notFound(w, r, u)
		return
	}
	if errors.Is(err, handset.ErrInvalid) {
		d, derr := s.handsetView(r, h)
		if derr != nil {
			s.internalError(w, r, derr)
			return
		}
		s.render(w, http.StatusBadRequest, "handset.html", page{Title: h.Display(), User: u, Data: d, Error: "That value is not allowed here."})
		return
	}
	s.internalError(w, r, err)
}

func (s *Server) handsetSave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	h, ok := s.handsetByPath(w, r, u)
	if !ok {
		return
	}
	up := handset.Update{Name: strings.TrimSpace(r.PostFormValue("name")), DualRing: r.PostFormValue("dual_ring")}
	if v := r.PostFormValue("default_line"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.handsetFail(w, r, u, h, handset.ErrInvalid)
			return
		}
		up.DefaultLine = &n
	}
	if err := s.Handsets.Update(r.Context(), u, h.ID, up); err != nil {
		s.handsetFail(w, r, u, h, err)
		return
	}
	http.Redirect(w, r, "/handsets/"+h.ID.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: our UUID
}

func (s *Server) handsetLine(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	h, ok := s.handsetByPath(w, r, u)
	if !ok {
		return
	}
	line, err := strconv.Atoi(r.PathValue("line"))
	if err != nil {
		s.handsetFail(w, r, u, h, handset.ErrInvalid)
		return
	}
	acct := uuid.Nil
	if v := r.PostFormValue("account"); v != "" {
		if acct, err = uuid.Parse(v); err != nil {
			s.handsetFail(w, r, u, h, handset.ErrInvalid)
			return
		}
	}
	if err := s.Handsets.SetLine(r.Context(), u, h.ID, line, acct); err != nil {
		s.handsetFail(w, r, u, h, err)
		return
	}
	http.Redirect(w, r, "/handsets/"+h.ID.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: our UUID
}
