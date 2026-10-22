// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
)

// SIPStatus is what the UI needs from the running SIP side.
type SIPStatus interface {
	Status(id uuid.UUID) (sip.RegStatus, bool)
}

func (s *Server) sipRoutes() {
	if s.SIPAccounts == nil {
		return
	}
	s.mux.Handle("GET /sip-accounts", s.requireUser(s.sipList))
	s.mux.Handle("GET /sip-accounts/new", s.requireUser(s.sipNew))
	s.mux.Handle("POST /sip-accounts", s.requireUser(s.sipCreate))
	s.mux.Handle("GET /sip-accounts/{id}", s.requireUser(s.sipEdit))
	s.mux.Handle("POST /sip-accounts/{id}", s.requireUser(s.sipUpdate))
	s.mux.Handle("POST /sip-accounts/{id}/delete", s.requireUser(s.sipDelete))
}

type sipRow struct {
	*sipaccount.Record
	Status   sip.RegStatus
	HasState bool
	Owner    string
}

func (s *Server) sipStatus(id uuid.UUID) (sip.RegStatus, bool) {
	if s.SIP == nil {
		return sip.RegStatus{}, false
	}
	return s.SIP.Status(id)
}

func (s *Server) ownerNames(r *http.Request, u *auth.User) map[uuid.UUID]string {
	names := map[uuid.UUID]string{}
	if !u.IsAdmin() {
		return names
	}
	if users, err := s.Auth.ListUsers(r.Context()); err == nil {
		for _, x := range users {
			names[x.ID] = x.Username
		}
	}
	return names
}

func (s *Server) sipList(w http.ResponseWriter, r *http.Request, u *auth.User) {
	recs, err := s.SIPAccounts.List(r.Context(), u, r.FormValue("owner"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	names := s.ownerNames(r, u)
	rows := make([]sipRow, len(recs))
	for i, rec := range recs {
		st, ok := s.sipStatus(rec.ID)
		owner := "shared"
		if rec.OwnerID != nil {
			owner = names[*rec.OwnerID]
		}
		rows[i] = sipRow{Record: rec, Status: st, HasState: ok, Owner: owner}
	}
	s.render(w, http.StatusOK, "sip_accounts.html", page{Title: "SIP accounts", User: u, Data: rows})
}

type sipForm struct {
	Rec      *sipaccount.Record
	New      bool
	Errors   sipaccount.ValidationError
	Users    []*auth.User
	Owner    string // "", "shared" or a user id
	Status   sip.RegStatus
	HasState bool
	Warnings []string
}

func (s *Server) renderSIPForm(w http.ResponseWriter, r *http.Request, u *auth.User, status int, f sipForm, p page) {
	if u.IsAdmin() {
		f.Users, _ = s.Auth.ListUsers(r.Context())
	}
	if f.Rec != nil && !f.New {
		f.Status, f.HasState = s.sipStatus(f.Rec.ID)
		f.Warnings = f.Rec.Warnings()
	}
	title := "New SIP account"
	if !f.New {
		title = f.Rec.Label
	}
	p.Title, p.User, p.Data = title, u, f
	s.render(w, status, "sip_account.html", p)
}

func (s *Server) sipNew(w http.ResponseWriter, r *http.Request, u *auth.User) {
	rec := &sipaccount.Record{Transport: "tls", Register: true, RegisterExpiry: 600, Keepalive: "auto", MediaEncryption: "optional",
		Codecs: []string{"G722", "PCMA", "PCMU"}, DTMFMode: "rfc4733", CallerIDSource: "auto", TLSVerify: true, Enabled: true}
	s.renderSIPForm(w, r, u, http.StatusOK, sipForm{Rec: rec, New: true}, page{})
}

// formInput reads the account form. The form always sends every field, so
// unchecked boxes mean false.
func formInput(r *http.Request, u *auth.User) (sipaccount.Input, error) {
	str := func(k string) *string { v := strings.TrimSpace(r.PostFormValue(k)); return &v }
	on := func(k string) *bool { v := r.PostFormValue(k) == "on"; return &v }
	in := sipaccount.Input{
		Label: str("label"), DisplayName: str("display_name"), Username: str("username"), Domain: str("domain"),
		AuthUsername: str("auth_username"), Registrar: str("registrar"), OutboundProxy: str("outbound_proxy"),
		Transport: str("transport"), Register: on("register"), Keepalive: str("keepalive"),
		MediaEncryption: str("media_encryption"), DTMFMode: str("dtmf_mode"), CallerIDSource: str("caller_id_source"),
		TLSVerify: on("tls_verify"), Enabled: on("enabled"),
	}
	n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("register_expiry")))
	if err != nil {
		return in, sipaccount.ValidationError{"register_expiry": "a number of seconds"}
	}
	in.RegisterExpiry = &n
	var codecs []string
	for _, c := range strings.Split(r.PostFormValue("codecs"), ",") {
		if c = strings.ToUpper(strings.TrimSpace(c)); c != "" {
			codecs = append(codecs, c)
		}
	}
	in.Codecs = &codecs
	adv := sipaccount.Advanced{UserAgent: strings.TrimSpace(r.PostFormValue("user_agent")), LinePrefix: strings.TrimSpace(r.PostFormValue("line_prefix"))}
	in.Advanced = &adv
	if pw := r.PostFormValue("password"); pw != "" {
		in.Password = &pw
	} else if r.PostFormValue("clear_password") == "on" {
		empty := ""
		in.Password = &empty
	}
	if u.IsAdmin() {
		switch o := r.PostFormValue("owner"); o {
		case "shared":
			var none *uuid.UUID
			in.OwnerID = &none
		case "":
		default:
			id, err := uuid.Parse(o)
			if err != nil {
				return in, sipaccount.ValidationError{"owner": "no such user"}
			}
			p := &id
			in.OwnerID = &p
		}
	}
	return in, nil
}

// formRecord shows what the user typed back in the form after an error.
func formRecord(r *http.Request) *sipaccount.Record {
	n, _ := strconv.Atoi(r.PostFormValue("register_expiry"))
	return &sipaccount.Record{Label: r.PostFormValue("label"), DisplayName: r.PostFormValue("display_name"), Username: r.PostFormValue("username"),
		Domain: r.PostFormValue("domain"), AuthUsername: r.PostFormValue("auth_username"), Registrar: r.PostFormValue("registrar"),
		OutboundProxy: r.PostFormValue("outbound_proxy"), Transport: r.PostFormValue("transport"), Register: r.PostFormValue("register") == "on",
		RegisterExpiry: n, Keepalive: r.PostFormValue("keepalive"), MediaEncryption: r.PostFormValue("media_encryption"),
		Codecs: strings.Split(r.PostFormValue("codecs"), ","), DTMFMode: r.PostFormValue("dtmf_mode"), CallerIDSource: r.PostFormValue("caller_id_source"),
		TLSVerify: r.PostFormValue("tls_verify") == "on", Enabled: r.PostFormValue("enabled") == "on",
		Advanced: sipaccount.Advanced{UserAgent: r.PostFormValue("user_agent"), LinePrefix: r.PostFormValue("line_prefix")}}
}

func (s *Server) sipCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	in, err := formInput(r, u)
	if err == nil {
		var rec *sipaccount.Record
		if rec, err = s.SIPAccounts.Create(r.Context(), u, in); err == nil {
			http.Redirect(w, r, "/sip-accounts/"+rec.ID.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: a UUID we generated
			return
		}
	}
	s.sipFormError(w, r, u, err, sipForm{Rec: formRecord(r), New: true, Owner: r.PostFormValue("owner")})
}

func (s *Server) sipFormError(w http.ResponseWriter, r *http.Request, u *auth.User, err error, f sipForm) {
	var v sipaccount.ValidationError
	switch {
	case errors.As(err, &v):
		f.Errors = v
		s.renderSIPForm(w, r, u, http.StatusBadRequest, f, page{Error: "Please correct the marked fields."})
	case errors.Is(err, auth.ErrForbidden):
		s.renderSIPForm(w, r, u, http.StatusForbidden, f, page{Error: "You may not do that."})
	case errors.Is(err, sipaccount.ErrNotFound):
		s.notFound(w, r, u)
	default:
		s.internalError(w, r, err)
	}
}

func (s *Server) sipByPath(w http.ResponseWriter, r *http.Request, u *auth.User) (*sipaccount.Record, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, u)
		return nil, false
	}
	rec, err := s.SIPAccounts.Get(r.Context(), u, id)
	if errors.Is(err, sipaccount.ErrNotFound) {
		s.notFound(w, r, u)
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return nil, false
	}
	return rec, true
}

func ownerValue(rec *sipaccount.Record) string {
	if rec.OwnerID == nil {
		return "shared"
	}
	return rec.OwnerID.String()
}

func (s *Server) sipEdit(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if rec, ok := s.sipByPath(w, r, u); ok {
		s.renderSIPForm(w, r, u, http.StatusOK, sipForm{Rec: rec, Owner: ownerValue(rec)}, page{Notice: r.FormValue("done")})
	}
}

func (s *Server) sipUpdate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	rec, ok := s.sipByPath(w, r, u)
	if !ok {
		return
	}
	in, err := formInput(r, u)
	if err == nil {
		if _, err = s.SIPAccounts.Update(r.Context(), u, rec.ID, in); err == nil {
			http.Redirect(w, r, "/sip-accounts/"+rec.ID.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: a UUID we generated
			return
		}
	}
	shown := formRecord(r)
	shown.ID, shown.HasPassword = rec.ID, rec.HasPassword
	s.sipFormError(w, r, u, err, sipForm{Rec: shown, Owner: r.PostFormValue("owner")})
}

func (s *Server) sipDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	rec, ok := s.sipByPath(w, r, u)
	if !ok {
		return
	}
	err := s.SIPAccounts.Delete(r.Context(), u, rec.ID)
	switch {
	case errors.Is(err, sipaccount.ErrInUse):
		s.renderSIPForm(w, r, u, http.StatusConflict, sipForm{Rec: rec, Owner: ownerValue(rec)},
			page{Error: "A handset line uses this account. Remove it from the handset first."})
	case err != nil:
		s.sipFormError(w, r, u, err, sipForm{Rec: rec})
	default:
		http.Redirect(w, r, "/sip-accounts", http.StatusSeeOther)
	}
}
