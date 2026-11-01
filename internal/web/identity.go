// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
)

// The network identity has a menu entry of its own: what the cells
// broadcast is what every handset knows them by, and a wrong click here can
// break the whole network, so it is not a button on the Cells page.
func (s *Server) identityRoutes() {
	if s.Cells == nil {
		return
	}
	s.mux.Handle("GET /network-identity", s.requireAdmin(s.identityList))
	s.mux.Handle("POST /network-identity", s.requireAdmin(s.identityPropose))
	s.mux.Handle("POST /network-identity/save", s.requireAdmin(s.identitySave))
	s.mux.Handle("POST /network-identity/apply", s.requireAdmin(s.identityApply))
	s.mux.Handle("POST /network-identity/donor", s.requireAdmin(s.identityDonor))
}

func (s *Server) identityList(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.renderIdentity(w, r, u, http.StatusOK, page{Notice: r.FormValue("done")})
}

// networkIdentityPage is the page's data: the system identity (nil: none) and
// the cells that could donate theirs.
type networkIdentityPage struct {
	Identity *cell.SystemIdentity
	Donors   []cellRow
}

func (s *Server) renderIdentity(w http.ResponseWriter, r *http.Request, u *auth.User, status int, p page) {
	id, err := s.Cells.SystemIdentity(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	cells, err := s.Cells.ListCells(r.Context(), u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var donors []cellRow
	for _, c := range cells {
		if live := s.liveStatus(c.ID); live.Online && live.HasRFPI {
			donors = append(donors, cellRow{c, live})
		}
	}
	p.Title, p.User, p.Data = "Network identity", u, networkIdentityPage{Identity: id, Donors: donors}
	s.render(w, status, "network_identity.html", p)
}

// identityPage is the confirmation step: the identity proposed, the one it
// would replace, and the risks (templates/identity.html).
type identityPage struct {
	Proposed cell.SystemIdentity
	Current  *cell.SystemIdentity
}

// ConfirmText is what is typed to confirm replacing the current identity.
func (p identityPage) ConfirmText() string { return p.Current.ConfirmText() }

// Replaces: saving the proposal would replace a different identity.
func (p identityPage) Replaces() bool {
	return p.Current != nil && (p.Current.EIC != p.Proposed.EIC || p.Current.FPN != p.Proposed.FPN || p.Current.FPS != p.Proposed.FPS)
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
		s.renderIdentity(w, r, u, http.StatusBadRequest, page{Error: err.Error()})
		return
	}
	cur, err := s.Cells.SystemIdentity(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "identity.html", page{Title: "Network identity", User: u,
		Data: identityPage{Proposed: id, Current: cur}})
}

func (s *Server) identitySave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	source := r.PostFormValue("source")
	if source != cell.IdentityRandom {
		source = cell.IdentityAllocated
	}
	id, err := identityForm(r, source)
	cur, _ := s.Cells.SystemIdentity(r.Context())
	replacing := cur != nil && (cur.EIC != id.EIC || cur.FPN != id.FPN || cur.FPS != id.FPS)
	switch {
	case err != nil:
	case cur != nil && !replacing:
		http.Redirect(w, r, "/network-identity?done=That+is+the+system+identity+already.", http.StatusSeeOther)
		return
	case r.PostFormValue("understood") != "on":
		err = errors.New("Tick the box to confirm you read the risks.") //nolint:staticcheck // ST1005: shown on the page as a sentence
	case replacing && r.PostFormValue("understood2") != "on":
		err = errors.New("Tick both boxes: replacing the identity of a running system breaks it.") //nolint:staticcheck // ST1005: as above
	case replacing && !strings.EqualFold(strings.TrimSpace(r.PostFormValue("confirm_eic")), cur.ConfirmText()):
		err = errors.New("Type the EIC of the identity being replaced, exactly as shown, to confirm.") //nolint:staticcheck // ST1005: as above
	}
	if err != nil {
		s.render(w, http.StatusBadRequest, "identity.html", page{Title: "Network identity", User: u,
			Error: err.Error(), Data: identityPage{Proposed: id, Current: cur}})
		return
	}
	if err := s.Cells.SetSystemIdentity(r.Context(), u, id); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/network-identity?done=System+identity+saved.", http.StatusSeeOther)
}

// identityApply turns the cells' use of the system identity on or off. On:
// every cell broadcasts it from its next connection, so every handset must
// register again; the admin confirms that.
func (s *Server) identityApply(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	on := r.PostFormValue("apply") == "on"
	if on && (r.PostFormValue("understood") != "on" || r.PostFormValue("understood2") != "on") {
		s.renderIdentity(w, r, u, http.StatusBadRequest, page{Error: "Tick both boxes to confirm that every handset has to register again."})
		return
	}
	if err := s.Cells.SetIdentityApply(r.Context(), u, on); err != nil {
		if errors.Is(err, cell.ErrBadIdentity) {
			s.renderIdentity(w, r, u, http.StatusBadRequest, page{Error: "There is no system identity to apply: set one first."})
			return
		}
		s.internalError(w, r, err)
		return
	}
	msg := "The+cells+take+their+own+identity+back+when+they+next+connect."
	if on {
		msg = "The+cells+take+the+system+identity+when+they+next+connect."
	}
	http.Redirect(w, r, "/network-identity?done="+msg, http.StatusSeeOther)
}

// identityDonor makes a cell's own identity the system's (class A, at most 7
// cells). Only while the system identity is not applied: until then every
// cell still broadcasts its factory identity, which is what is taken.
func (s *Server) identityDonor(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	id, err := uuid.Parse(r.PostFormValue("cell"))
	if err != nil {
		s.renderIdentity(w, r, u, http.StatusBadRequest, page{Error: "Pick a cell."})
		return
	}
	c, err := s.Cells.GetCell(r.Context(), u, id)
	if errors.Is(err, cell.ErrNotFound) {
		s.renderIdentity(w, r, u, http.StatusBadRequest, page{Error: "That cell does not exist."})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
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
		s.renderIdentity(w, r, u, http.StatusBadRequest, page{Error: "The cells use the system identity now. Give them their own identity back first, wait until they have reloaded, then take a cell's."})
		return
	case cur != nil && (r.PostFormValue("understood2") != "on" || !strings.EqualFold(strings.TrimSpace(r.PostFormValue("confirm_eic")), cur.ConfirmText())):
		s.renderIdentity(w, r, u, http.StatusBadRequest, page{Error: "Replacing the system identity can break the network: tick both boxes and type " + cur.ConfirmText() + ", the identity being replaced, to confirm."})
		return
	case r.PostFormValue("understood") != "on":
		s.renderIdentity(w, r, u, http.StatusBadRequest, page{Error: "Tick the box to confirm that you read what this does."})
		return
	case !live.Online || !live.HasRFPI:
		s.renderIdentity(w, r, u, http.StatusBadRequest, page{Error: "That cell is offline or does not report its identity (older software)."})
		return
	}
	if err := s.Cells.SystemIdentityFromCell(r.Context(), u, live.RFPI); err != nil {
		if errors.Is(err, cell.ErrBadIdentity) {
			s.renderIdentity(w, r, u, http.StatusBadRequest, page{Error: "That cell's identity is not a class A one; take another cell, or use a class B identity."})
			return
		}
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/network-identity?done=System+identity+taken+from+the+cell.", http.StatusSeeOther)
}
