// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/org"
)

// The API of organizations and of the trust setting (docs/adr/0007-organizations-and-trust.md); the same store as the pages.

func (s *Server) apiOrgRoutes(h func(string, func(http.ResponseWriter, *http.Request, *auth.User))) {
	h("GET "+apiPrefix+"/me/trust", s.apiTrustGet)
	h("PUT "+apiPrefix+"/me/trust", s.apiTrustPut)
	h("GET "+apiPrefix+"/organizations", s.apiOrgList)
	h("POST "+apiPrefix+"/organizations", s.apiOrgCreate)
	h("POST "+apiPrefix+"/organizations/invitation-preview", s.apiInvitePreview)
	h("POST "+apiPrefix+"/organizations/join", s.apiOrgJoin)
	h("GET "+apiPrefix+"/organizations/{id}", s.apiOrgGet)
	h("PATCH "+apiPrefix+"/organizations/{id}", s.apiOrgPatch)
	h("DELETE "+apiPrefix+"/organizations/{id}", s.apiOrgDelete)
	h("POST "+apiPrefix+"/organizations/{id}/invitations", s.apiOrgInvite)
	h("DELETE "+apiPrefix+"/organizations/{id}/membership", s.apiOrgLeave)
	h("PATCH "+apiPrefix+"/organizations/{id}/members/{user}", s.apiMemberPatch)
	h("DELETE "+apiPrefix+"/organizations/{id}/members/{user}", s.apiMemberDelete)
}

type apiOrgJSON struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role,omitempty"`
	Owner       bool      `json:"owner"`
	MemberCount int       `json:"member_count"`
	Trusted     bool      `json:"trusted"`
	CreatedAt   time.Time `json:"created_at"`
}

func orgJSON(m org.Mine) apiOrgJSON {
	return apiOrgJSON{ID: m.ID, Name: m.Name, DisplayName: m.Display, Role: string(m.Role), Owner: m.Owner,
		MemberCount: m.Members, Trusted: m.Trusted, CreatedAt: m.CreatedAt}
}

type apiCountsJSON struct {
	Total   int `json:"total"`
	Online  int `json:"online"`
	Offline int `json:"offline"`
}

func countsJSON(c org.Counts) apiCountsJSON {
	return apiCountsJSON{Total: c.Total, Online: c.Online, Offline: c.Offline()}
}

type apiMemberJSON struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"name"`
	Role   string    `json:"role"`
	Owner  bool      `json:"owner"`
	Cells  int       `json:"cells"`
	Joined time.Time `json:"joined_at"`
}

type apiOrgOverviewJSON struct {
	apiOrgJSON
	Cells           apiCountsJSON   `json:"cells"`
	Handsets        apiCountsJSON   `json:"handsets"`
	People          []apiMemberJSON `json:"people"`
	CanInvite       bool            `json:"can_invite"`
	CanRemove       bool            `json:"can_remove"`
	CanAppoint      bool            `json:"can_appoint"`
	CanDelete       bool            `json:"can_delete"`
	OpenInvitations int             `json:"open_invitations"`
}

func overviewJSON(o *org.Overview) apiOrgOverviewJSON {
	out := apiOrgOverviewJSON{apiOrgJSON: orgJSON(o.Mine), Cells: countsJSON(o.Cells), Handsets: countsJSON(o.Handsets),
		People: make([]apiMemberJSON, len(o.People)), CanInvite: o.CanInvite, CanRemove: o.CanRemove,
		CanAppoint: o.CanAppoint, CanDelete: o.CanDelete, OpenInvitations: o.OpenInvites}
	for i, m := range o.People {
		out.People[i] = apiMemberJSON{UserID: m.UserID, Name: m.Name, Role: string(m.Role), Owner: m.Owner, Cells: m.Cells, Joined: m.Joined}
	}
	return out
}

// apiOrgError answers an error of the organization store.
func (s *Server) apiOrgError(w http.ResponseWriter, r *http.Request, err error) {
	var v org.ValidationError
	var stranded *org.StrandedError
	switch {
	case errors.Is(err, org.ErrNotFound):
		notFoundAPI(w)
	case errors.Is(err, org.ErrForbidden):
		problem(w, http.StatusForbidden, "forbidden", "you may see this organization but not do that", nil)
	case errors.As(err, &v):
		problem(w, http.StatusBadRequest, "invalid", v.Error(), nil)
	case errors.Is(err, org.ErrBadToken):
		problem(w, http.StatusBadRequest, "bad_invitation", "the invitation is not valid: it may have expired or been used", nil)
	case errors.Is(err, org.ErrBadScope):
		problem(w, http.StatusBadRequest, "invalid", "scope is one of all, own, orgs", nil)
	case errors.Is(err, org.ErrAlreadyMember):
		problem(w, http.StatusConflict, "already_member", "you are already a member", nil)
	case errors.Is(err, org.ErrTooMany):
		problem(w, http.StatusConflict, "too_many", "a limit was reached: organizations per user, members, or open invitations", nil)
	case errors.Is(err, org.ErrOwner):
		problem(w, http.StatusConflict, "owner", "the owner cannot be removed, demoted or leave; delete the organization instead", nil)
	case errors.As(err, &stranded):
		problem(w, http.StatusConflict, "no_usable_cell", "with this setting none of your handsets can use a base station; send confirm: true to save it anyway", nil)
	default:
		s.apiError(w, r, err)
	}
}

type apiTrustJSON struct {
	Scope           string      `json:"scope"`
	OrganizationIDs []uuid.UUID `json:"organization_ids"`
	UsableCells     int         `json:"usable_cells"`
	Handsets        int         `json:"handsets"`
	Warnings        []string    `json:"warnings"`
}

func (s *Server) trustJSON(r *http.Request, u *auth.User) (apiTrustJSON, error) {
	t, err := s.Orgs.GetTrust(r.Context(), u)
	if err != nil {
		return apiTrustJSON{}, err
	}
	reach, err := s.Orgs.Reach(r.Context(), u.ID)
	if err != nil {
		return apiTrustJSON{}, err
	}
	out := apiTrustJSON{Scope: string(t.Scope), OrganizationIDs: []uuid.UUID{}, UsableCells: reach.Cells, Handsets: reach.Handsets, Warnings: []string{}}
	for id := range t.Orgs {
		out.OrganizationIDs = append(out.OrganizationIDs, id)
	}
	if reach.Stranded() {
		out.Warnings = append(out.Warnings, "no_usable_cell")
	}
	return out, nil
}

func (s *Server) apiTrustGet(w http.ResponseWriter, r *http.Request, u *auth.User) {
	out, err := s.trustJSON(r, u)
	if err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiTrustPut(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var in struct {
		Scope           string      `json:"scope"`
		OrganizationIDs []uuid.UUID `json:"organization_ids"`
		Confirm         bool        `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	save := s.Orgs.SetTrustChecked
	if in.Confirm {
		save = s.Orgs.SetTrust
	}
	if err := save(r.Context(), u, org.Scope(in.Scope), in.OrganizationIDs); err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	s.apiTrustGet(w, r, u)
}

func (s *Server) apiOrgList(w http.ResponseWriter, r *http.Request, u *auth.User) {
	mine, err := s.Orgs.ListMine(r.Context(), u)
	if err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	items := make([]apiOrgJSON, len(mine))
	for i, m := range mine {
		items[i] = orgJSON(m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next": nil})
}

func (s *Server) apiOrgCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	o, err := s.Orgs.Create(r.Context(), u, in.Name)
	if err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, apiOrgJSON{ID: o.ID, Name: o.Name, DisplayName: o.Name, Role: string(org.RoleAdmin), Owner: true,
		MemberCount: 1, CreatedAt: o.CreatedAt})
}

func (s *Server) apiOrgPathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		notFoundAPI(w)
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) apiOrgOverview(w http.ResponseWriter, r *http.Request, u *auth.User, id uuid.UUID, status int) {
	ov, err := s.Orgs.Overview(r.Context(), u, id, s.onlineCells, s.handsetCounts)
	if err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	writeJSON(w, status, overviewJSON(ov))
}

func (s *Server) apiOrgGet(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if id, ok := s.apiOrgPathID(w, r); ok {
		s.apiOrgOverview(w, r, u, id, http.StatusOK)
	}
}

// apiOrgPatch renames the organization (its owner) and/or sets the caller's own name for it (any member).
func (s *Server) apiOrgPatch(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.apiOrgPathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Name        *string `json:"name"`
		DisplayName *string `json:"display_name"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Name == nil && in.DisplayName == nil {
		problem(w, http.StatusBadRequest, "invalid", "give name and/or display_name", nil)
		return
	}
	// check the right to look at it first, so a stranger learns nothing from the order of the errors
	if _, err := s.Orgs.Overview(r.Context(), u, id, nil, nil); err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	if in.Name != nil {
		if err := s.Orgs.Rename(r.Context(), u, id, *in.Name); err != nil {
			s.apiOrgError(w, r, err)
			return
		}
	}
	if in.DisplayName != nil {
		if err := s.Orgs.SetAlias(r.Context(), u, id, *in.DisplayName); err != nil {
			s.apiOrgError(w, r, err)
			return
		}
	}
	s.apiOrgOverview(w, r, u, id, http.StatusOK)
}

func (s *Server) apiOrgDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.apiOrgPathID(w, r)
	if !ok {
		return
	}
	if err := s.Orgs.Delete(r.Context(), u, id); err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiOrgInvite(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.apiOrgPathID(w, r)
	if !ok {
		return
	}
	tok, exp, err := s.Orgs.CreateInvite(r.Context(), u, id)
	if err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	link := strings.TrimRight(s.BaseURL.String(), "/") + "/organizations/join?token=" + url.QueryEscape(tok)
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "join_url": link, "expires_at": exp})
}

func (s *Server) apiInvitePreview(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var in struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &in) {
		return
	}
	o, err := s.Orgs.PeekInvite(r.Context(), in.Token)
	if err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"organization_name": o.Name})
}

func (s *Server) apiOrgJoin(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var in struct {
		Token       string `json:"token"`
		DisplayName string `json:"display_name"`
	}
	if !decode(w, r, &in) {
		return
	}
	o, err := s.Orgs.Join(r.Context(), u, in.Token, in.DisplayName)
	if err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	s.apiOrgOverview(w, r, u, o.ID, http.StatusOK)
}

func (s *Server) apiOrgLeave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.apiOrgPathID(w, r)
	if !ok {
		return
	}
	if err := s.Orgs.Leave(r.Context(), u, id); err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiMemberPatch(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.apiOrgPathID(w, r)
	if !ok {
		return
	}
	m, err := uuid.Parse(r.PathValue("user"))
	if err != nil {
		notFoundAPI(w)
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Orgs.SetRole(r.Context(), u, id, m, org.Role(in.Role)); err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	s.apiOrgOverview(w, r, u, id, http.StatusOK)
}

func (s *Server) apiMemberDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.apiOrgPathID(w, r)
	if !ok {
		return
	}
	m, err := uuid.Parse(r.PathValue("user"))
	if err != nil {
		notFoundAPI(w)
		return
	}
	if err := s.Orgs.Remove(r.Context(), u, id, m); err != nil {
		s.apiOrgError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
