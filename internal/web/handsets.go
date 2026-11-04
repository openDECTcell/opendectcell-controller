// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/profiles"
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
	s.mux.Handle("POST /handsets/{id}/deregister", s.requireUser(s.handsetDeregister))
	s.mux.Handle("GET /admin/handset-release", s.requireAdmin(s.releasePage))
	s.mux.Handle("POST /admin/handset-release", s.requireAdmin(s.releaseFind))
	s.mux.Handle("POST /admin/handset-release/confirm", s.requireAdmin(s.releaseConfirm))
}

type releaseData struct {
	IPEI  string
	Claim *handset.Claim
}

func (s *Server) releasePage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.render(w, http.StatusOK, "admin_release.html", page{Title: "Release a handset identity", User: u, Data: releaseData{}, Notice: r.FormValue("done")})
}

// releaseFind looks a handset up by its IPEI and shows whose it is, so an administrator can release it.
func (s *Server) releaseFind(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	ipei := strings.TrimSpace(r.PostFormValue("ipei"))
	c, err := s.Handsets.FindClaim(r.Context(), u, ipei)
	d := releaseData{IPEI: ipei, Claim: c}
	switch {
	case errors.Is(err, handset.ErrBadIPEI):
		s.render(w, http.StatusBadRequest, "admin_release.html", page{Title: "Release a handset identity", User: u, Data: d, Error: err.Error() + "."})
	case errors.Is(err, handset.ErrNotFound):
		s.render(w, http.StatusNotFound, "admin_release.html", page{Title: "Release a handset identity", User: u, Data: d, Error: "No handset is registered under this IPEI."})
	case err != nil:
		s.internalError(w, r, err)
	default:
		s.render(w, http.StatusOK, "admin_release.html", page{Title: "Release a handset identity", User: u, Data: d})
	}
}

// releaseConfirm removes the handset once the IPEI is typed again: the cells drop its record at once, then its row goes.
func (s *Server) releaseConfirm(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	id, err := uuid.Parse(r.PostFormValue("id"))
	if err != nil {
		s.notFound(w, r, u)
		return
	}
	c, err := s.Handsets.FindClaim(r.Context(), u, r.PostFormValue("ipei"))
	if err != nil || c.ID != id { // the IPEI typed is the one of this handset, or nothing happens
		s.render(w, http.StatusBadRequest, "admin_release.html", page{Title: "Release a handset identity", User: u, Data: releaseData{},
			Error: "The IPEI typed does not match the handset. Nothing was released."})
		return
	}
	if s.HandsetDeregister != nil {
		s.HandsetDeregister(id, true)
	}
	if err := s.Handsets.Release(r.Context(), u, id); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/handset-release?done=The+handset+is+released.+Its+owner+and+every+base+station+forgot+it.", http.StatusSeeOther)
}

func (s *Server) handsetList(w http.ResponseWriter, r *http.Request, u *auth.User) {
	hs, err := s.Handsets.List(r.Context(), u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "handsets.html", page{Title: "Handsets", User: u, Data: hs, Notice: r.FormValue("done"), Warn: s.strandedWarn(r, u.ID)})
}

// profileMaker is one manufacturer with its models, for the pickers (a
// manufacturer first, then a model, as in the LuCI app).
type profileMaker struct {
	ID, Name string
	Models   []profiles.Profile
}

func profileMakers() []profileMaker {
	makers := profiles.Default.Manufacturers()
	out := make([]profileMaker, 0, len(makers))
	for _, m := range makers {
		out = append(out, profileMaker{ID: m.ID, Name: m.Name, Models: profiles.Default.Models(m.ID)})
	}
	return out
}

type registerPage struct {
	Available bool
	Makers    []profileMaker
}

func (s *Server) handsetRegister(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.render(w, http.StatusOK, "handset_register.html", page{Title: "Register a handset", User: u,
		Data: registerPage{Available: s.Registrations != nil, Makers: profileMakers()}})
}

type handsetPage struct {
	H        *handset.Handset
	Accounts []handset.Account
	Line     map[int]*handset.Line
	Moves    []handset.Move
	Makers   []profileMaker
}

func (s *Server) handsetView(r *http.Request, h *handset.Handset) (handsetPage, error) {
	accts, err := s.Handsets.Accounts(r.Context(), h.OwnerID)
	d := handsetPage{H: h, Accounts: accts, Line: map[int]*handset.Line{}, Makers: profileMakers()}
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
	s.render(w, http.StatusOK, "handset.html", page{Title: h.Display(), User: u, Data: d, Notice: r.FormValue("done"), Warn: s.strandedWarn(r, h.OwnerID)})
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
	up := handset.Update{Name: strings.TrimSpace(r.PostFormValue("name")), DualRing: r.PostFormValue("dual_ring"), Disclosed: r.PostFormValue("disclosed") == "on"}
	// the form carries both lines; a client that sends neither leaves them as they are
	for _, n := range []int{1, 2} {
		v, present := r.PostForm["account"+strconv.Itoa(n)]
		if !present || len(v) != 1 {
			continue
		}
		acct := uuid.Nil
		if v[0] != "" {
			var err error
			if acct, err = uuid.Parse(v[0]); err != nil {
				s.handsetFail(w, r, u, h, handset.ErrInvalid)
				return
			}
		}
		if up.Lines == nil {
			up.Lines = map[int]uuid.UUID{}
		}
		up.Lines[n] = acct
	}
	if v := r.PostFormValue("default_line"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.handsetFail(w, r, u, h, handset.ErrInvalid)
			return
		}
		// a line the same save clears cannot stay the default
		if acct, cleared := up.Lines[n]; !cleared || acct != uuid.Nil {
			up.DefaultLine = &n
		}
	}
	if v, ok := r.PostForm["profile"]; ok && len(v) == 1 {
		if v[0] != "" && !profileKnown(v[0]) {
			s.handsetFail(w, r, u, h, handset.ErrInvalid)
			return
		}
		up.Profile, up.Model = &v[0], profiles.Default.Label(v[0])
	}
	if err := s.Handsets.Update(r.Context(), u, h.ID, up); err != nil {
		s.handsetFail(w, r, u, h, err)
		return
	}
	if s.HandsetChanged != nil && (up.Name != h.Name || (up.Profile != nil && *up.Profile != h.Profile)) {
		s.HandsetChanged(h.ID)
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

// handsetDeregister removes the handset: the cells are told to drop its record
// (the one that holds it asks the handset first, unless force: it may be out of
// range), then its row, lines and history go. The handset is registered again
// as a new one, which makes it a handset of every cell.
func (s *Server) handsetDeregister(w http.ResponseWriter, r *http.Request, u *auth.User) {
	h, ok := s.handsetByPath(w, r, u)
	if !ok {
		return
	}
	force := r.PostFormValue("force") == "1"
	if s.HandsetDeregister != nil {
		s.HandsetDeregister(h.ID, force)
	}
	if err := s.Handsets.Delete(r.Context(), u, h.ID); err != nil {
		s.handsetFail(w, r, u, h, err)
		return
	}
	msg := "The+handset+is+removed.+The+cells+ask+it+to+deregister;+one+that+is+out+of+range+is+forgotten+after+about+30+seconds.+Delete+the+base+on+the+handset+too."
	if force {
		msg = "The+handset+is+removed+and+the+cells+forget+it+now.+Delete+the+base+on+the+handset+too."
	}
	http.Redirect(w, r, "/handsets?done="+msg, http.StatusSeeOther)
}
