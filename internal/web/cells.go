// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
)

// CellLink is what the web UI needs from the live cell links.
type CellLink interface {
	CellStatus(id uuid.UUID) cell.Status
	NotifyState(id uuid.UUID, st wire.CellStateValue, reason string)
	Kick(id uuid.UUID)
}

func (s *Server) cellRoutes() {
	if s.Cells == nil {
		return
	}
	s.mux.Handle("GET /cells", s.requireAdmin(s.cellList))
	s.mux.Handle("GET /cells/{id}", s.requireAdmin(s.cellPage))
	s.mux.Handle("POST /cells/{id}/state", s.requireAdmin(s.cellState))
	s.mux.Handle("GET /enroll", s.requireUser(s.enrollPage))
	s.mux.Handle("POST /enroll", s.requireUser(s.enrollCreate))
	s.mux.Handle("POST /enroll/{id}/delete", s.requireUser(s.enrollDelete))
}

type cellRow struct {
	*cell.Cell
	Live cell.Status
}

func (s *Server) liveStatus(id uuid.UUID) cell.Status {
	if s.CellLink == nil {
		return cell.Status{}
	}
	return s.CellLink.CellStatus(id)
}

func (s *Server) cellList(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.renderCells(w, r, u, http.StatusOK, page{Notice: r.FormValue("done")})
}

func (s *Server) renderCells(w http.ResponseWriter, r *http.Request, u *auth.User, status int, p page) {
	cells, err := s.Cells.ListCells(r.Context(), u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	rows := make([]cellRow, len(cells))
	for i, c := range cells {
		rows[i] = cellRow{c, s.liveStatus(c.ID)}
	}
	p.Title, p.User, p.Data = "All base stations", u, cellsPage{Cells: rows}
	s.render(w, status, "cells.html", p)
}

type cellsPage struct {
	Cells []cellRow
}

func (s *Server) cellByPath(w http.ResponseWriter, r *http.Request, u *auth.User) (*cell.Cell, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, u)
		return nil, false
	}
	c, err := s.Cells.GetCell(r.Context(), u, id)
	if errors.Is(err, cell.ErrNotFound) {
		s.notFound(w, r, u)
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return nil, false
	}
	return c, true
}

func (s *Server) renderCell(w http.ResponseWriter, status int, u *auth.User, c *cell.Cell, p page) {
	p.Title, p.User, p.Data = c.Name, u, cellRow{c, s.liveStatus(c.ID)}
	s.render(w, status, "cell.html", p)
}

func (s *Server) cellPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if c, ok := s.cellByPath(w, r, u); ok {
		s.renderCell(w, http.StatusOK, u, c, page{Notice: r.FormValue("done")})
	}
}

func (s *Server) cellState(w http.ResponseWriter, r *http.Request, u *auth.User) {
	c, ok := s.cellByPath(w, r, u)
	if !ok {
		return
	}
	to := r.PostFormValue("state")
	st, err := s.Cells.SetState(r.Context(), u, c.ID, to)
	if err != nil {
		s.renderCell(w, http.StatusConflict, u, c, page{Error: "The cell's state cannot be changed that way."})
		return
	}
	if s.CellLink != nil {
		s.CellLink.NotifyState(c.ID, st, "changed by "+u.Username)
	}
	http.Redirect(w, r, "/cells/"+c.ID.String()+"?done=State+changed.", http.StatusSeeOther)
}

type enrollData struct {
	Tokens     []*cell.Token
	Enrollment string
	IsAdmin    bool
	Now        time.Time
}

func (s *Server) renderEnroll(w http.ResponseWriter, r *http.Request, u *auth.User, status int, p page, es string) {
	toks, err := s.Cells.ListTokens(r.Context(), u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	p.Title, p.User = "Enroll a cell", u
	p.Data = enrollData{Tokens: toks, Enrollment: es, IsAdmin: u.IsAdmin(), Now: time.Now()}
	s.render(w, status, "enroll.html", p)
}

func (s *Server) enrollPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if !auth.CanAddCells(u) {
		s.notFound(w, r, u)
		return
	}
	s.renderEnroll(w, r, u, http.StatusOK, page{}, "")
}

var tokenTTLs = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

func (s *Server) enrollCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !auth.CanAddCells(u) {
		s.notFound(w, r, u)
		return
	}
	ttl, ok := tokenTTLs[r.PostFormValue("ttl")]
	if !ok {
		ttl = cell.DefaultTokenTTL
	}
	_, es, err := s.Cells.CreateToken(r.Context(), u, r.PostFormValue("note"), ttl)
	if errors.Is(err, auth.ErrForbidden) {
		s.renderEnroll(w, r, u, http.StatusForbidden, page{Error: "Only administrators can create auto-approving tokens."}, "")
		return
	}
	if err != nil {
		s.renderEnroll(w, r, u, http.StatusBadRequest, page{Error: err.Error()}, "")
		return
	}
	// Shown once: the page is not cached, and nothing stores the string.
	s.renderEnroll(w, r, u, http.StatusOK, page{}, es)
}

func (s *Server) enrollDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil || !auth.CanAddCells(u) {
		s.notFound(w, r, u)
		return
	}
	if err := s.Cells.DeleteToken(r.Context(), u, id); errors.Is(err, cell.ErrNotFound) {
		s.notFound(w, r, u)
		return
	} else if err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/enroll", http.StatusSeeOther)
}
