// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
)

// The JSON API (docs/api/openapi.yaml) for the resources that exist so
// far. It uses the browser session and the same CSRF protection as the
// pages, and the same store functions, so the authorisation rules are the
// same code. A request may carry a personal API token instead of a session (apiscopes.go): then only what the token's scopes allow.

const apiPrefix = "/api/v1"

func (s *Server) apiRoutes() {
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request, *auth.User)) {
		s.mux.Handle(pattern, s.apiUser(fn))
	}
	h("GET "+apiPrefix+"/me", s.apiMe)
	h("GET "+apiPrefix+"/users", s.apiAdmin(s.apiUsers))
	h("POST "+apiPrefix+"/users", s.apiAdmin(s.apiUserCreate))
	h("GET "+apiPrefix+"/users/{id}", s.apiAdmin(s.apiUserGet))
	h("PATCH "+apiPrefix+"/users/{id}", s.apiAdmin(s.apiUserPatch))
	h("DELETE "+apiPrefix+"/users/{id}", s.apiAdmin(s.apiUserDelete))
	h("GET "+apiPrefix+"/audit", s.apiAdmin(s.apiAudit))
	s.resourceRoutes()
	if s.Orgs != nil {
		s.apiOrgRoutes(h)
	}
	if s.SIPAccounts != nil {
		h("GET "+apiPrefix+"/sip-accounts", s.apiSIPList)
		h("POST "+apiPrefix+"/sip-accounts", s.apiSIPCreate)
		h("GET "+apiPrefix+"/sip-accounts/{id}", s.apiSIPGet)
		h("PATCH "+apiPrefix+"/sip-accounts/{id}", s.apiSIPPatch)
		h("DELETE "+apiPrefix+"/sip-accounts/{id}", s.apiSIPDelete)
		h("GET "+apiPrefix+"/sip-accounts/{id}/status", s.apiSIPStatus)
	}
	if s.Cells != nil {
		h("GET "+apiPrefix+"/cells", s.apiCells)
		h("GET "+apiPrefix+"/cells/{id}", s.apiAdmin(s.apiCellGet))
		h("PATCH "+apiPrefix+"/cells/{id}", s.apiAdmin(s.apiCellPatch))
		h("POST "+apiPrefix+"/cells/{id}/revoke", s.apiAdmin(s.apiCellRevoke))
		h("GET "+apiPrefix+"/enrollment-tokens", s.apiTokens)
		h("POST "+apiPrefix+"/enrollment-tokens", s.apiTokenCreate)
		h("DELETE "+apiPrefix+"/enrollment-tokens/{id}", s.apiTokenDelete)
	}
}

// problem writes an RFC 9457 problem document.
func problem(w http.ResponseWriter, status int, code, detail string, fields map[string]string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "code": code}
	if detail != "" {
		body["detail"] = detail
	}
	if len(fields) > 0 {
		body["fields"] = fields
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFoundAPI(w http.ResponseWriter) { problem(w, http.StatusNotFound, "not_found", "", nil) }

// apiUser answers 401 (not a redirect) to anonymous API requests.
func (s *Server) apiUser(h func(http.ResponseWriter, *http.Request, *auth.User)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFrom(r.Context())
		if u == nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			problem(w, http.StatusUnauthorized, "unauthenticated", "log in first, or send a personal API token", nil)
			return
		}
		if u.LegalPending {
			problem(w, http.StatusForbidden, "legal_acceptance_required", "sign off the legal texts in the web interface (/legal/accept) before you use the API", nil)
			return
		}
		if tok, ok := tokenFrom(r.Context()); ok && !tokenAllows(w, r, tok) {
			return
		}
		h(w, r, u)
	})
}

// apiAdmin answers 404 to non-admins, like the admin pages, and 403 to a local admin who has not set up
// two-factor sign-in where the settings require it (as requireAdmin redirects the pages): the API must
// not be the way round the requirement.
func (s *Server) apiAdmin(h func(http.ResponseWriter, *http.Request, *auth.User)) func(http.ResponseWriter, *http.Request, *auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u *auth.User) {
		if u.Admin2FAPending {
			problem(w, http.StatusForbidden, "two_factor_required", "set up two-factor sign-in at /account/2fa before using the admin API", nil)
			return
		}
		if auth.RequireAdmin(u) != nil {
			notFoundAPI(w)
			return
		}
		h(w, r, u)
	}
}

// decode reads a JSON body of at most 64 KiB, rejecting unknown fields.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		problem(w, http.StatusBadRequest, "invalid", "body: "+err.Error(), nil)
		return false
	}
	// A request is exactly one JSON value; anything after it is refused.
	if _, err := dec.Token(); err != io.EOF {
		problem(w, http.StatusBadRequest, "invalid", "body: more than one JSON value", nil)
		return false
	}
	return true
}

func (s *Server) apiError(w http.ResponseWriter, r *http.Request, err error) {
	var v sipaccount.ValidationError
	switch {
	case errors.As(err, &v):
		problem(w, http.StatusBadRequest, "invalid", "", v)
	case errors.Is(err, sipaccount.ErrNotFound), errors.Is(err, cell.ErrNotFound), errors.Is(err, auth.ErrNotFound):
		notFoundAPI(w)
	case errors.Is(err, sipaccount.ErrInUse):
		problem(w, http.StatusConflict, "in_use", err.Error(), nil)
	case errors.Is(err, auth.ErrHasObjects):
		problem(w, http.StatusConflict, "has_objects", err.Error(), nil)
	case errors.Is(err, auth.ErrUsernameTaken):
		problem(w, http.StatusConflict, "exists", err.Error(), nil)
	case errors.Is(err, auth.ErrSelf):
		problem(w, http.StatusConflict, "self", err.Error(), nil)
	case errors.Is(err, auth.ErrForbidden):
		problem(w, http.StatusForbidden, "forbidden", "", nil)
	case errors.Is(err, cell.ErrBadState):
		problem(w, http.StatusConflict, "bad_state", err.Error(), nil)
	default:
		s.Log.Error("API request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		problem(w, http.StatusInternalServerError, "internal", "", nil)
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		notFoundAPI(w)
	}
	return id, err == nil
}

// users

type apiUserJSON struct {
	ID          uuid.UUID  `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"display_name"`
	Email       string     `json:"email"`
	Role        string     `json:"role"`
	MayAddCells bool       `json:"may_add_cells"`
	AuthSource  string     `json:"auth_source"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
}

func userJSON(u *auth.User) apiUserJSON {
	return apiUserJSON{u.ID, u.Username, u.DisplayName, u.Email, string(u.Role), u.MayAddCells, u.AuthSource, u.Disabled, u.CreatedAt, u.LastLoginAt}
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request, u *auth.User) {
	writeJSON(w, http.StatusOK, userJSON(u))
}

func (s *Server) apiUsers(w http.ResponseWriter, r *http.Request, u *auth.User) {
	users, err := s.Auth.ListUsers(r.Context())
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	items := make([]apiUserJSON, len(users))
	for i, x := range users {
		items[i] = userJSON(x)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next": nil})
}

func (s *Server) apiUserCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var in struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
		Password    string `json:"password"`
		Role        string `json:"role"`
		MayAddCells bool   `json:"may_add_cells"`
	}
	if !decode(w, r, &in) {
		return
	}
	nu, err := s.Auth.CreateLocalUser(r.Context(), auth.NewLocalUser{Username: in.Username, DisplayName: in.DisplayName,
		Email: in.Email, Password: in.Password, Role: auth.Role(in.Role), MayAddCells: in.MayAddCells})
	if err != nil && !errors.Is(err, auth.ErrUsernameTaken) {
		problem(w, http.StatusBadRequest, "invalid", err.Error(), nil)
		return
	}
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, userJSON(nu))
}

func (s *Server) apiUserGet(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	t, err := s.Auth.UserByID(r.Context(), s.Pool, id)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, userJSON(t))
}

func (s *Server) apiUserPatch(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		DisplayName *string `json:"display_name"`
		Email       *string `json:"email"`
		Role        *string `json:"role"`
		MayAddCells *bool   `json:"may_add_cells"`
		Disabled    *bool   `json:"disabled"`
		Password    *string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	up := auth.UserUpdate{DisplayName: in.DisplayName, Email: in.Email, MayAddCells: in.MayAddCells, Disabled: in.Disabled}
	if in.Role != nil {
		role := auth.Role(*in.Role)
		up.Role = &role
	}
	var was bool
	if before, berr := s.Auth.UserByID(r.Context(), s.Pool, id); berr == nil {
		was = before.Disabled
	}
	t, err := s.Auth.UpdateUser(r.Context(), u, id, up)
	if err == nil && up.Disabled != nil && *up.Disabled && !was {
		s.disableCellsOf(r.Context(), id)
	}
	if err == nil && in.Password != nil {
		if t.AuthSource != "local" {
			problem(w, http.StatusConflict, "managed_externally", "this user signs in with single sign-on", nil)
			return
		}
		err = s.Auth.SetPassword(r.Context(), id, *in.Password, nil)
	}
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, userJSON(t))
}

func (s *Server) apiUserDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Auth.DeleteUser(r.Context(), u, id); err != nil {
		s.apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request, u *auth.User) {
	f := audit.Filter{ObjectType: r.FormValue("object_type"), ObjectID: r.FormValue("object_id")}
	f.Limit, _ = strconv.Atoi(r.FormValue("limit"))
	f.Before, _ = strconv.ParseInt(r.FormValue("before"), 10, 64)
	if a := r.FormValue("actor_id"); a != "" {
		id, err := uuid.Parse(a)
		if err != nil {
			problem(w, http.StatusBadRequest, "invalid", "actor_id", nil)
			return
		}
		f.ActorID = &id
	}
	rows, err := audit.List(r.Context(), s.Pool, f)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	type entry struct {
		ID         int64          `json:"id"`
		At         time.Time      `json:"at"`
		ActorID    *uuid.UUID     `json:"actor_id"`
		ActorName  string         `json:"actor_name"`
		RemoteAddr *string        `json:"remote_addr"`
		Action     string         `json:"action"`
		ObjectType string         `json:"object_type"`
		ObjectID   string         `json:"object_id"`
		Details    map[string]any `json:"details"`
	}
	items := make([]entry, len(rows))
	for i, x := range rows {
		e := entry{x.ID, x.At, x.ActorID, x.ActorName, nil, x.Action, x.ObjectType, x.ObjectID, x.Details}
		if x.RemoteAddr != nil {
			a := x.RemoteAddr.String()
			e.RemoteAddr = &a
		}
		items[i] = e
	}
	var next *string
	if len(rows) > 0 && (f.Limit == 0 && len(rows) == 50 || len(rows) == f.Limit) {
		n := strconv.FormatInt(rows[len(rows)-1].ID, 10)
		next = &n
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next": next})
}

// SIP accounts

type apiSIPJSON struct {
	ID              uuid.UUID           `json:"id"`
	OwnerID         *uuid.UUID          `json:"owner_id"`
	Label           string              `json:"label"`
	DisplayName     string              `json:"display_name"`
	Username        string              `json:"username"`
	Domain          string              `json:"domain"`
	AuthUsername    *string             `json:"auth_username"`
	HasPassword     bool                `json:"has_password"`
	HasStunPass     bool                `json:"has_stunpass"`
	Registrar       *string             `json:"registrar"`
	OutboundProxy   *string             `json:"outbound_proxy"`
	Transport       string              `json:"transport"`
	Register        bool                `json:"register"`
	RegisterExpiry  int                 `json:"register_expiry"`
	Keepalive       string              `json:"keepalive"`
	MediaEncryption string              `json:"media_encryption"`
	Codecs          []string            `json:"codecs"`
	DTMFMode        string              `json:"dtmf_mode"`
	CallerIDSource  string              `json:"caller_id_source"`
	TLSVerify       bool                `json:"tls_verify"`
	Advanced        sipaccount.Advanced `json:"advanced"`
	Enabled         bool                `json:"enabled"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	Warnings        []string            `json:"warnings"`
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sipJSON(a *sipaccount.Record) apiSIPJSON {
	w := a.Warnings()
	if w == nil {
		w = []string{}
	}
	return apiSIPJSON{a.ID, a.OwnerID, a.Label, a.DisplayName, a.Username, a.Domain, nilIfEmpty(a.AuthUsername), a.HasPassword, a.HasStunPass,
		nilIfEmpty(a.Registrar), nilIfEmpty(a.OutboundProxy), a.Transport, a.Register, a.RegisterExpiry, a.Keepalive,
		a.MediaEncryption, a.Codecs, a.DTMFMode, a.CallerIDSource, a.TLSVerify, a.Advanced, a.Enabled, a.CreatedAt, a.UpdatedAt, w}
}

// sipInput is SipAccountInput. owner_id needs three states (absent, null,
// a user), so it is decoded separately.
type sipInput struct {
	Label           *string              `json:"label"`
	DisplayName     *string              `json:"display_name"`
	Username        *string              `json:"username"`
	Domain          *string              `json:"domain"`
	AuthUsername    *string              `json:"auth_username"`
	Password        *string              `json:"password"`
	StunPass        *string              `json:"stunpass"`
	Registrar       *string              `json:"registrar"`
	OutboundProxy   *string              `json:"outbound_proxy"`
	Transport       *string              `json:"transport"`
	Register        *bool                `json:"register"`
	RegisterExpiry  *int                 `json:"register_expiry"`
	Keepalive       *string              `json:"keepalive"`
	MediaEncryption *string              `json:"media_encryption"`
	Codecs          *[]string            `json:"codecs"`
	DTMFMode        *string              `json:"dtmf_mode"`
	CallerIDSource  *string              `json:"caller_id_source"`
	TLSVerify       *bool                `json:"tls_verify"`
	TLSCAPEM        *string              `json:"tls_ca_pem"`
	Advanced        *sipaccount.Advanced `json:"advanced"`
	Enabled         *bool                `json:"enabled"`
	OwnerID         json.RawMessage      `json:"owner_id"`
}

func (in sipInput) toStore() (sipaccount.Input, error) {
	out := sipaccount.Input{Label: in.Label, DisplayName: in.DisplayName, Username: in.Username, Domain: in.Domain,
		AuthUsername: in.AuthUsername, Password: in.Password, StunPass: in.StunPass, Registrar: in.Registrar, OutboundProxy: in.OutboundProxy,
		Transport: in.Transport, Register: in.Register, RegisterExpiry: in.RegisterExpiry, Keepalive: in.Keepalive,
		MediaEncryption: in.MediaEncryption, Codecs: in.Codecs, DTMFMode: in.DTMFMode, CallerIDSource: in.CallerIDSource,
		TLSVerify: in.TLSVerify, Advanced: in.Advanced, Enabled: in.Enabled}
	if len(in.OwnerID) > 0 {
		var id *uuid.UUID
		if err := json.Unmarshal(in.OwnerID, &id); err != nil {
			return out, sipaccount.ValidationError{"owner_id": "a user id or null"}
		}
		out.OwnerID = &id
	}
	return out, nil
}

func (s *Server) apiSIPList(w http.ResponseWriter, r *http.Request, u *auth.User) {
	recs, err := s.SIPAccounts.List(r.Context(), u, r.FormValue("owner"))
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	items := make([]apiSIPJSON, len(recs))
	for i, a := range recs {
		items[i] = sipJSON(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next": nil})
}

func (s *Server) apiSIPCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var in sipInput
	if !decode(w, r, &in) {
		return
	}
	si, err := in.toStore()
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	a, err := s.SIPAccounts.Create(r.Context(), u, si)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sipJSON(a))
}

func (s *Server) apiSIPGet(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a, err := s.SIPAccounts.Get(r.Context(), u, id)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sipJSON(a))
}

func (s *Server) apiSIPPatch(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in sipInput
	if !decode(w, r, &in) {
		return
	}
	si, err := in.toStore()
	if err == nil {
		var a *sipaccount.Record
		if a, err = s.SIPAccounts.Update(r.Context(), u, id, si); err == nil {
			writeJSON(w, http.StatusOK, sipJSON(a))
			return
		}
	}
	s.apiError(w, r, err)
}

func (s *Server) apiSIPDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.SIPAccounts.Delete(r.Context(), u, id); err != nil {
		s.apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiSIPStatus(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := s.SIPAccounts.Get(r.Context(), u, id); err != nil {
		s.apiError(w, r, err)
		return
	}
	st, running := s.sipStatus(id)
	state := string(st.State)
	if !running {
		state = "disabled"
	}
	var until *time.Time
	if !st.RegisteredUntil.IsZero() {
		until = &st.RegisteredUntil
	}
	writeJSON(w, http.StatusOK, map[string]any{"registration": state, "registered_until": until,
		"last_error": nilIfEmpty(st.LastError), "server": nilIfEmpty(st.Server), "transport": string(st.Transport)})
}

// cells and enrollment

func (s *Server) cellJSON(c *cell.Cell, full bool) map[string]any {
	m := map[string]any{"id": c.ID, "name": c.Name, "location": c.Location, "state": c.State}
	if !full {
		return m
	}
	live := s.liveStatus(c.ID)
	m["online"], m["health"] = live.Online, live.Health()
	if live.Online {
		m["rtt_p95_ms"] = live.RTTp95.Milliseconds()
	} else {
		m["rtt_p95_ms"] = nil
	}
	m["enrolled_at"], m["approved_at"] = c.EnrolledAt, c.ApprovedAt
	m["enrolled_by"] = nil // the operator is shown only for a base station they disclosed to the administrators
	if c.Disclosed {
		m["enrolled_by"] = c.EnrolledBy
	}
	m["software_version"], m["stub_version"] = c.SoftwareVersion, c.StubVersion
	m["capacity"] = map[string]any{"sub_records": c.CapSubRecords, "audio_nb": c.CapAudioNB, "audio_wb": c.CapAudioWB}
	m["certificate_expires_at"], m["last_seen_at"] = c.CertExpiresAt, c.LastSeenAt
	return m
}

func (s *Server) apiCells(w http.ResponseWriter, r *http.Request, u *auth.User) {
	cells, err := s.Cells.ListCells(r.Context(), u)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(cells))
	for _, c := range cells {
		if st := r.FormValue("state"); st != "" && st != c.State {
			continue
		}
		items = append(items, s.cellJSON(c, u.IsAdmin()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next": nil})
}

func (s *Server) apiCellGet(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	c, err := s.Cells.GetCell(r.Context(), u, id)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.cellJSON(c, true))
}

func (s *Server) apiCellPatch(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Name     *string `json:"name"`
		Location *string `json:"location"`
		State    *string `json:"state"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Name != nil || in.Location != nil {
		if _, err := s.Cells.Update(r.Context(), u, id, cell.CellUpdate{Name: in.Name, Location: in.Location}); err != nil {
			if errors.Is(err, cell.ErrNotFound) {
				notFoundAPI(w)
			} else {
				problem(w, http.StatusBadRequest, "invalid", err.Error(), nil)
			}
			return
		}
	}
	if in.State != nil {
		st, err := s.Cells.SetState(r.Context(), u, id, *in.State)
		if err != nil {
			s.apiError(w, r, err)
			return
		}
		if s.CellLink != nil {
			s.CellLink.NotifyState(id, st, "changed by "+u.Username)
		}
	}
	s.apiCellGet(w, r, u)
}

func (s *Server) apiCellRevoke(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Cells.Revoke(r.Context(), u, id); err != nil {
		s.apiError(w, r, err)
		return
	}
	if s.CellLink != nil {
		s.CellLink.Kick(id)
	}
	s.apiCellGet(w, r, u)
}

func tokenJSON(t *cell.Token) map[string]any {
	return map[string]any{"id": t.ID, "note": t.Note, "created_by": t.CreatedBy, "created_at": t.CreatedAt,
		"expires_at": t.ExpiresAt, "auto_approve": t.AutoApprove, "used_at": t.UsedAt, "cell_id": t.CellID}
}

func (s *Server) apiTokens(w http.ResponseWriter, r *http.Request, u *auth.User) {
	toks, err := s.Cells.ListTokens(r.Context(), u)
	if errors.Is(err, auth.ErrForbidden) {
		notFoundAPI(w)
		return
	}
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	items := make([]map[string]any, len(toks))
	for i, t := range toks {
		items[i] = tokenJSON(t)
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) apiTokenCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if !auth.CanAddCells(u) {
		notFoundAPI(w)
		return
	}
	var in struct {
		Note        string `json:"note"`
		TTLSeconds  int    `json:"ttl_seconds"`
		AutoApprove bool   `json:"auto_approve"` // accepted and ignored: a cell is always approved by its token
	}
	if !decode(w, r, &in) {
		return
	}
	t, es, err := s.Cells.CreateToken(r.Context(), u, in.Note, time.Duration(in.TTLSeconds)*time.Second)
	if errors.Is(err, auth.ErrForbidden) {
		problem(w, http.StatusForbidden, "forbidden", "you may not enroll cells", nil)
		return
	}
	if err != nil {
		problem(w, http.StatusBadRequest, "invalid", err.Error(), nil)
		return
	}
	m := tokenJSON(t)
	m["enrollment_string"] = es
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) apiTokenDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Cells.DeleteToken(r.Context(), u, id); err != nil {
		s.apiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
