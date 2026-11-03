// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/org"
)

// The pages of organizations and of the trust setting (docs/adr/0007-organizations-and-trust.md).
func (s *Server) orgRoutes() {
	if s.Orgs == nil {
		return
	}
	s.mux.Handle("GET /organizations", s.requireUser(s.orgList))
	s.mux.Handle("POST /organizations", s.requireUser(s.orgCreate))
	s.mux.Handle("POST /organizations/trust", s.requireUser(s.trustSave))
	s.mux.Handle("GET /organizations/join", s.requireUser(s.orgJoinForm))
	s.mux.Handle("POST /organizations/join", s.requireUser(s.orgJoin))
	s.mux.Handle("GET /organizations/{id}", s.requireUser(s.orgPage))
	s.mux.Handle("POST /organizations/{id}/alias", s.requireUser(s.orgAlias))
	s.mux.Handle("POST /organizations/{id}/rename", s.requireUser(s.orgRename))
	s.mux.Handle("POST /organizations/{id}/invite", s.requireUser(s.orgInvite))
	s.mux.Handle("POST /organizations/{id}/members/{user}/remove", s.requireUser(s.orgRemove))
	s.mux.Handle("POST /organizations/{id}/members/{user}/role", s.requireUser(s.orgRole))
	s.mux.Handle("POST /organizations/{id}/leave", s.requireUser(s.orgLeave))
	s.mux.Handle("POST /organizations/{id}/delete", s.requireUser(s.orgDelete))
}

type orgListData struct {
	Orgs  []org.Mine
	Trust org.Trust
	All   []org.Mine // every organization, for a controller admin
	// Confirm is set when the setting just sent would leave the user's handsets without a base station:
	// nothing was saved, and the page asks.
	Confirm *trustConfirm
}

// trustConfirm is the setting that was refused until the user confirms it.
type trustConfirm struct {
	Scope    org.Scope
	Orgs     []uuid.UUID
	Handsets int
}

// strandedWarning is the text shown where a user whose handsets have no base station looks.
func strandedWarning(r org.Reach) string {
	who := map[bool]string{true: "handset", false: fmt.Sprintf("%d handsets", r.Handsets)}[r.Handsets == 1]
	if r.Scope == org.ScopeAll {
		return fmt.Sprintf("None of your %s can use a base station: this controller has no active base station at the moment, so they cannot be reached or make calls.", who)
	}
	return fmt.Sprintf("None of your %s can use a base station: with your trust setting there is no active base station they may be put on, so they cannot be reached or make calls. Choose a wider setting under Organizations and trust, or enroll a base station of your own.", who)
}

// strandedWarn is the warning for the user's own handsets, or "" when they can reach a base station (or there are none).
func (s *Server) strandedWarn(r *http.Request, user uuid.UUID) string {
	if s.Orgs == nil {
		return ""
	}
	reach, err := s.Orgs.Reach(r.Context(), user)
	if err != nil || !reach.Stranded() {
		return ""
	}
	return strandedWarning(reach)
}

func (s *Server) orgListView(r *http.Request, u *auth.User) (orgListData, error) {
	var d orgListData
	var err error
	if d.Orgs, err = s.Orgs.ListMine(r.Context(), u); err != nil {
		return d, err
	}
	if d.Trust, err = s.Orgs.GetTrust(r.Context(), u); err != nil {
		return d, err
	}
	if u.IsAdmin() {
		d.All, err = s.Orgs.ListAll(r.Context(), u)
	}
	return d, err
}

func (s *Server) renderOrgList(w http.ResponseWriter, r *http.Request, u *auth.User, status int, errMsg string) {
	s.renderOrgListWith(w, r, u, status, errMsg, nil)
}

func (s *Server) renderOrgListWith(w http.ResponseWriter, r *http.Request, u *auth.User, status int, errMsg string, confirm *trustConfirm) {
	d, err := s.orgListView(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	d.Confirm = confirm
	warn := ""
	if confirm == nil { // when asking, the question says it
		warn = s.strandedWarn(r, u.ID)
	}
	s.render(w, status, "organizations.html", page{Title: "Organizations and trust", User: u, Data: d, Error: errMsg, Notice: r.FormValue("done"), Warn: warn})
}

func (s *Server) orgList(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.renderOrgList(w, r, u, http.StatusOK, "")
}

// orgMessage turns an error of the store into words for the user, or "" for one that is ours.
func orgMessage(err error) string {
	var v org.ValidationError
	switch {
	case errors.As(err, &v):
		return v.Error()
	case errors.Is(err, org.ErrBadToken), errors.Is(err, org.ErrAlreadyMember), errors.Is(err, org.ErrTooMany),
		errors.Is(err, org.ErrOwner), errors.Is(err, org.ErrForbidden):
		m := err.Error()
		return strings.ToUpper(m[len("org: "):][:1]) + m[len("org: "):][1:] + "."
	}
	return ""
}

func (s *Server) orgCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	o, err := s.Orgs.Create(r.Context(), u, r.PostFormValue("name"))
	if err != nil {
		if m := orgMessage(err); m != "" {
			s.renderOrgList(w, r, u, http.StatusBadRequest, m)
			return
		}
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/organizations/"+o.ID.String()+"?done="+url.QueryEscape("Organization created. You are its owner."), http.StatusSeeOther) //nolint:gosec // G710: our UUID
}

func (s *Server) trustSave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		s.renderOrgList(w, r, u, http.StatusBadRequest, "That form could not be read.")
		return
	}
	var ids []uuid.UUID
	for _, v := range r.PostForm["org"] {
		if id, err := uuid.Parse(v); err == nil {
			ids = append(ids, id)
		}
	}
	scope := org.Scope(r.PostFormValue("scope"))
	save := s.Orgs.SetTrustChecked
	if r.PostFormValue("confirm") == "1" {
		save = s.Orgs.SetTrust // the user has seen the warning
	}
	if err := save(r.Context(), u, scope, ids); err != nil {
		var stranded *org.StrandedError
		switch {
		case errors.Is(err, org.ErrBadScope):
			s.renderOrgList(w, r, u, http.StatusBadRequest, "Choose one of the three settings.")
		case errors.As(err, &stranded):
			s.renderOrgListWith(w, r, u, http.StatusConflict, "", &trustConfirm{Scope: scope, Orgs: ids, Handsets: stranded.Handsets})
		default:
			s.internalError(w, r, err)
		}
		return
	}
	http.Redirect(w, r, "/organizations?done="+url.QueryEscape("Saved. Handsets that are no longer allowed on a base station are taken off it."), http.StatusSeeOther)
}

func (s *Server) orgID(w http.ResponseWriter, r *http.Request, u *auth.User) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, u)
		return uuid.Nil, false
	}
	return id, true
}

// orgFail answers an error of an action on one organization.
func (s *Server) orgFail(w http.ResponseWriter, r *http.Request, u *auth.User, id uuid.UUID, err error) {
	if errors.Is(err, org.ErrNotFound) {
		s.notFound(w, r, u)
		return
	}
	if m := orgMessage(err); m != "" {
		s.renderOrg(w, r, u, id, http.StatusBadRequest, m, "", "")
		return
	}
	s.internalError(w, r, err)
}

type orgPageData struct {
	O *org.Overview
	// Token and JoinURL are shown once, right after an invitation was made.
	Token, JoinURL string
	Expires        time.Time
}

func (s *Server) onlineCells(id uuid.UUID) bool { return s.liveStatus(id).Online }

func (s *Server) handsetCounts(ctx context.Context, owners []uuid.UUID) (org.Counts, error) {
	if s.Handsets == nil {
		return org.Counts{}, nil
	}
	total, online, err := s.Handsets.PresenceCounts(ctx, owners)
	return org.Counts{Total: total, Online: online}, err
}

func (s *Server) renderOrg(w http.ResponseWriter, r *http.Request, u *auth.User, id uuid.UUID, status int, errMsg, token, joinURL string) {
	ov, err := s.Orgs.Overview(r.Context(), u, id, s.onlineCells, s.handsetCounts)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			s.notFound(w, r, u)
			return
		}
		s.internalError(w, r, err)
		return
	}
	d := orgPageData{O: ov, Token: token, JoinURL: joinURL}
	if token != "" {
		d.Expires = time.Now().Add(org.InviteTTL)
	}
	s.render(w, status, "organization.html", page{Title: ov.Display, User: u, Data: d, Error: errMsg, Notice: r.FormValue("done")})
}

func (s *Server) orgPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if id, ok := s.orgID(w, r, u); ok {
		s.renderOrg(w, r, u, id, http.StatusOK, "", "", "")
	}
}

func (s *Server) orgAlias(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	id, ok := s.orgID(w, r, u)
	if !ok {
		return
	}
	if err := s.Orgs.SetAlias(r.Context(), u, id, r.PostFormValue("alias")); err != nil {
		s.orgFail(w, r, u, id, err)
		return
	}
	http.Redirect(w, r, "/organizations/"+id.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: our UUID
}

func (s *Server) orgRename(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	id, ok := s.orgID(w, r, u)
	if !ok {
		return
	}
	if err := s.Orgs.Rename(r.Context(), u, id, r.PostFormValue("name")); err != nil {
		s.orgFail(w, r, u, id, err)
		return
	}
	http.Redirect(w, r, "/organizations/"+id.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: our UUID
}

func (s *Server) orgInvite(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.orgID(w, r, u)
	if !ok {
		return
	}
	tok, _, err := s.Orgs.CreateInvite(r.Context(), u, id)
	if err != nil {
		s.orgFail(w, r, u, id, err)
		return
	}
	link := strings.TrimRight(s.BaseURL.String(), "/") + "/organizations/join?token=" + url.QueryEscape(tok)
	s.renderOrg(w, r, u, id, http.StatusOK, "", tok, link)
}

func (s *Server) memberID(w http.ResponseWriter, r *http.Request, u *auth.User) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("user"))
	if err != nil {
		s.notFound(w, r, u)
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) orgRemove(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.orgID(w, r, u)
	if !ok {
		return
	}
	m, ok := s.memberID(w, r, u)
	if !ok {
		return
	}
	if err := s.Orgs.Remove(r.Context(), u, id, m); err != nil {
		s.orgFail(w, r, u, id, err)
		return
	}
	http.Redirect(w, r, "/organizations/"+id.String()+"?done=Removed.", http.StatusSeeOther) //nolint:gosec // G710: our UUID
}

func (s *Server) orgRole(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	id, ok := s.orgID(w, r, u)
	if !ok {
		return
	}
	m, ok := s.memberID(w, r, u)
	if !ok {
		return
	}
	if err := s.Orgs.SetRole(r.Context(), u, id, m, org.Role(r.PostFormValue("role"))); err != nil {
		s.orgFail(w, r, u, id, err)
		return
	}
	http.Redirect(w, r, "/organizations/"+id.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: our UUID
}

func (s *Server) orgLeave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.orgID(w, r, u)
	if !ok {
		return
	}
	if err := s.Orgs.Leave(r.Context(), u, id); err != nil {
		s.orgFail(w, r, u, id, err)
		return
	}
	http.Redirect(w, r, "/organizations?done=You+left+the+organization.", http.StatusSeeOther)
}

func (s *Server) orgDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	id, ok := s.orgID(w, r, u)
	if !ok {
		return
	}
	ov, err := s.Orgs.Overview(r.Context(), u, id, nil, nil)
	if err != nil {
		s.orgFail(w, r, u, id, err)
		return
	}
	if r.PostFormValue("confirm") != ov.Name {
		s.renderOrg(w, r, u, id, http.StatusBadRequest, "Type the organization's name to confirm that you want to delete it.", "", "")
		return
	}
	if err := s.Orgs.Delete(r.Context(), u, id); err != nil {
		s.orgFail(w, r, u, id, err)
		return
	}
	http.Redirect(w, r, "/organizations?done=Organization+deleted.", http.StatusSeeOther)
}

type joinData struct {
	Org   *org.Org
	Token string
}

// orgJoinForm shows which organization a token is for and asks for the name the user wants to call it by,
// suggesting the name its owner gave it. Nothing is used up until the user confirms.
func (s *Server) orgJoinForm(w http.ResponseWriter, r *http.Request, u *auth.User) {
	tok := strings.TrimSpace(r.URL.Query().Get("token"))
	if tok == "" {
		s.render(w, http.StatusOK, "organization_join.html", page{Title: "Join an organization", User: u, Data: joinData{}})
		return
	}
	o, err := s.Orgs.PeekInvite(r.Context(), tok)
	if err != nil {
		s.render(w, http.StatusBadRequest, "organization_join.html", page{Title: "Join an organization", User: u, Data: joinData{}, Error: orgMessage(err)})
		return
	}
	s.render(w, http.StatusOK, "organization_join.html", page{Title: "Join " + o.Name, User: u, Data: joinData{Org: o, Token: tok}})
}

func (s *Server) orgJoin(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	tok := strings.TrimSpace(r.PostFormValue("token"))
	// a first step with only a token typed in: show the confirmation
	if r.PostFormValue("confirm") == "" {
		http.Redirect(w, r, "/organizations/join?token="+url.QueryEscape(tok), http.StatusSeeOther)
		return
	}
	o, err := s.Orgs.Join(r.Context(), u, tok, r.PostFormValue("alias"))
	if err != nil {
		if m := orgMessage(err); m != "" {
			var peeked *org.Org
			if p, perr := s.Orgs.PeekInvite(r.Context(), tok); perr == nil {
				peeked = p
			}
			s.render(w, http.StatusBadRequest, "organization_join.html", page{Title: "Join an organization", User: u, Data: joinData{Org: peeked, Token: tok}, Error: m})
			return
		}
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/organizations/"+o.ID.String()+"?done="+url.QueryEscape("You joined “"+o.Name+"”."), http.StatusSeeOther) //nolint:gosec // G710: our UUID
}
