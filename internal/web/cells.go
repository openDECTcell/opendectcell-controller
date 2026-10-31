// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strconv"
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
	s.mux.Handle("POST /cells/identity", s.requireAdmin(s.identityPropose))
	s.mux.Handle("POST /cells/identity/save", s.requireAdmin(s.identitySave))
	s.mux.Handle("POST /cells/identity/apply", s.requireAdmin(s.identityApply))
	s.mux.Handle("POST /cells/{id}/identity-donor", s.requireAdmin(s.identityDonor))
	s.mux.Handle("GET /cells/{id}", s.requireAdmin(s.cellPage))
	s.mux.Handle("POST /cells/{id}", s.requireAdmin(s.cellUpdate))
	s.mux.Handle("POST /cells/{id}/state", s.requireAdmin(s.cellState))
	s.mux.Handle("POST /cells/{id}/revoke", s.requireAdmin(s.cellRevoke))
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
	id, err := s.Cells.SystemIdentity(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	p.Title, p.User, p.Data = "Cells", u, cellsPage{Cells: rows, Identity: id}
	s.render(w, status, "cells.html", p)
}

type cellsPage struct {
	Cells    []cellRow
	Identity *cell.SystemIdentity
}

// identityPage is the confirmation step: the identity proposed, the one it
// would replace, and the risks (templates/identity.html).
type identityPage struct {
	Proposed cell.SystemIdentity
	Current  *cell.SystemIdentity
}

// identityForm reads an identity from a form: EIC in hex, FPN and FPS in
// decimal, as the page shows them.
func identityForm(r *http.Request, source string) (cell.SystemIdentity, error) {
	eic, err := strconv.ParseUint(r.PostFormValue("eic"), 16, 16)
	if err != nil {
		return cell.SystemIdentity{}, errors.New("The EIC is four hexadecimal digits.") //nolint:staticcheck // ST1005: shown on the page as a sentence
	}
	fpn, err := strconv.ParseUint(r.PostFormValue("fpn"), 10, 8)
	if err != nil {
		return cell.SystemIdentity{}, errors.New("The FPN is a number from 0 to 255.") //nolint:staticcheck // ST1005: as above
	}
	fps, err := strconv.ParseUint(r.PostFormValue("fps"), 10, 8)
	if err != nil {
		return cell.SystemIdentity{}, errors.New("The FPS is a number from 1 to 15.") //nolint:staticcheck // ST1005: as above
	}
	id := cell.SystemIdentity{EIC: uint16(eic), FPN: uint8(fpn), FPS: uint8(fps), Source: source}
	if err := id.Validate(); err != nil {
		return id, errors.New("EIC 0 is not used, and the FPS is 1 to 15 (EN 300 175-6 §5.2).") //nolint:staticcheck // ST1005: as above
	}
	return id, nil
}

// identityPropose: "Make one up" draws a random identity, "Use an EIC from
// ETSI" takes the one typed in; either is only shown, with the risks,
// until the admin saves it.
func (s *Server) identityPropose(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var (
		id  cell.SystemIdentity
		err error
	)
	if r.PostFormValue("action") == "random" {
		id, err = cell.RandomIdentity(nil)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
	} else if id, err = identityForm(r, cell.IdentityAllocated); err != nil {
		s.renderCells(w, r, u, http.StatusBadRequest, page{Error: err.Error()})
		return
	}
	cur, err := s.Cells.SystemIdentity(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "identity.html", page{Title: "DECT system identity", User: u,
		Data: identityPage{Proposed: id, Current: cur}})
}

func (s *Server) identitySave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	source := r.PostFormValue("source")
	if source != cell.IdentityRandom {
		source = cell.IdentityAllocated
	}
	id, err := identityForm(r, source)
	if err == nil && r.PostFormValue("understood") != "on" {
		err = errors.New("Tick the box to confirm you read the risks.") //nolint:staticcheck // ST1005: shown on the page as a sentence
	}
	if err != nil {
		cur, _ := s.Cells.SystemIdentity(r.Context())
		s.render(w, http.StatusBadRequest, "identity.html", page{Title: "DECT system identity", User: u,
			Error: err.Error(), Data: identityPage{Proposed: id, Current: cur}})
		return
	}
	if err := s.Cells.SetSystemIdentity(r.Context(), u, id); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/cells?done=System+identity+saved.", http.StatusSeeOther)
}

// identityApply turns the cells' use of the system identity on or off. On:
// every cell broadcasts it from its next connection, so every handset must
// register again; the admin confirms that.
func (s *Server) identityApply(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	on := r.PostFormValue("apply") == "on"
	if on && r.PostFormValue("understood") != "on" {
		s.renderCells(w, r, u, http.StatusBadRequest, page{Error: "Tick the box to confirm that every handset has to register again."})
		return
	}
	if err := s.Cells.SetIdentityApply(r.Context(), u, on); err != nil {
		if errors.Is(err, cell.ErrBadIdentity) {
			s.renderCells(w, r, u, http.StatusBadRequest, page{Error: "There is no system identity to apply: set one first."})
			return
		}
		s.internalError(w, r, err)
		return
	}
	msg := "The+cells+take+their+own+identity+back+when+they+next+connect."
	if on {
		msg = "The+cells+take+the+system+identity+when+they+next+connect."
	}
	http.Redirect(w, r, "/cells?done="+msg, http.StatusSeeOther)
}

// identityDonor makes a cell's own identity the system's (class A, at most 7
// cells). Only while the system identity is not applied: until then every
// cell still broadcasts its factory identity, which is what is taken.
func (s *Server) identityDonor(w http.ResponseWriter, r *http.Request, u *auth.User) {
	c, ok := s.cellByPath(w, r, u)
	if !ok {
		return
	}
	cur, err := s.Cells.SystemIdentity(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	live := s.liveStatus(c.ID)
	switch {
	case cur != nil && cur.Apply:
		s.renderCells(w, r, u, http.StatusBadRequest, page{Error: "The system identity is applied; turn that off first, so the cells broadcast their own again."})
		return
	case !live.Online || !live.HasRFPI:
		s.renderCells(w, r, u, http.StatusBadRequest, page{Error: "That cell is offline or does not report its identity (older software)."})
		return
	}
	if err := s.Cells.SystemIdentityFromCell(r.Context(), u, live.RFPI); err != nil {
		if errors.Is(err, cell.ErrBadIdentity) {
			s.renderCells(w, r, u, http.StatusBadRequest, page{Error: "That cell's identity is not a class A one; take another cell, or use a class B identity."})
			return
		}
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/cells?done=System+identity+taken+from+the+cell.", http.StatusSeeOther)
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

func (s *Server) cellUpdate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	c, ok := s.cellByPath(w, r, u)
	if !ok {
		return
	}
	name, loc := r.PostFormValue("name"), r.PostFormValue("location")
	plain := r.PostFormValue("allow_plain_rtp") == "on"
	up := cell.CellUpdate{Name: &name, Location: &loc, AllowPlainRTP: &plain}
	if v := r.PostFormValue("max_calls"); v != "" {
		n, err := strconv.ParseInt(v, 10, 16)
		if err != nil {
			s.renderCell(w, http.StatusBadRequest, u, c, page{Error: "Max calls must be a number."})
			return
		}
		m := int16(n)
		up.MaxCalls = &m
	}
	nc, err := s.Cells.Update(r.Context(), u, c.ID, up)
	if err != nil {
		if errors.Is(err, cell.ErrNotFound) {
			s.notFound(w, r, u)
			return
		}
		s.renderCell(w, http.StatusBadRequest, u, c, page{Error: err.Error()})
		return
	}
	http.Redirect(w, r, "/cells/"+nc.ID.String()+"?done=Saved.", http.StatusSeeOther)
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

func (s *Server) cellRevoke(w http.ResponseWriter, r *http.Request, u *auth.User) {
	c, ok := s.cellByPath(w, r, u)
	if !ok {
		return
	}
	if r.PostFormValue("confirm") != c.Name {
		s.renderCell(w, http.StatusBadRequest, u, c, page{Error: "Type the cell's name to confirm revocation."})
		return
	}
	if err := s.Cells.Revoke(r.Context(), u, c.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	if s.CellLink != nil {
		s.CellLink.Kick(c.ID)
	}
	http.Redirect(w, r, "/cells/"+c.ID.String()+"?done=Revoked.", http.StatusSeeOther)
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
	_, es, err := s.Cells.CreateToken(r.Context(), u, r.PostFormValue("note"), ttl, r.PostFormValue("auto_approve") == "on")
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
