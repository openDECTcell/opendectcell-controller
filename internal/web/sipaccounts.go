// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/numberplan"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
	"github.com/openDECTcell/opendectcell-controller/internal/sipevents"
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
	Server   string               // what the account is at: the registrar, else the domain
	Uses     []sipaccount.LineUse // the lines of the user's own handsets it serves
}

// serverOf is the host the user thinks of as the registrar: the override if
// the account has one, else the SIP domain (the same thing for most accounts).
func serverOf(r *sipaccount.Record) string {
	if r.Registrar != "" {
		return r.Registrar
	}
	return r.Domain
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
	ids := make([]uuid.UUID, len(recs))
	for i, rec := range recs {
		ids[i] = rec.ID
	}
	uses, err := s.SIPAccounts.Uses(r.Context(), u, ids)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	rows := make([]sipRow, len(recs))
	for i, rec := range recs {
		st, ok := s.sipStatus(rec.ID)
		owner := "shared"
		if rec.OwnerID != nil {
			owner = names[*rec.OwnerID]
		}
		rows[i] = sipRow{Record: rec, Status: st, HasState: ok, Owner: owner, Server: serverOf(rec), Uses: uses[rec.ID]}
	}
	s.render(w, http.StatusOK, "sip_accounts.html", page{Title: "VoIP lines", User: u, Data: rows})
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
	History  []sipevents.Event
	Summary  sipevents.Summary
	Calls    []callRow
	Tab      string          // the tab the page opens on: the first with an error, else Account
	TabErr   map[string]bool // tabs holding a marked field
}

// sipTabs lists the tabs of the form, as the LuCI VoIP profile pop-up names them.
var sipTabs = []struct{ ID, Title string }{{"acct", "Account"}, {"net", "Server and NAT"}, {"media", "Media"}, {"enc", "Encryption"}, {"num", "Numbers"}, {"adv", "Advanced"}}

// sipFieldTab says which tab a field with an error is on.
func sipFieldTab(field string) string {
	switch field {
	case "label", "display_name", "username", "auth_username", "password", "registrar", "domain", "register_expiry", "advanced.line_name":
		return "acct"
	case "transport", "outbound_proxy", "advanced.outbound2", "advanced.medianat", "advanced.stunserver", "advanced.stun_server",
		"advanced.stunuser", "stunpass", "advanced.rtpkeep":
		return "net"
	case "codecs", "advanced.ptime", "dtmf_mode", "advanced.dtmf", "advanced.rel100":
		return "media"
	case "media_encryption", "advanced.mediaenc":
		return "enc"
	case "advanced.numbers":
		return "num"
	case "account", "":
		return ""
	}
	return "adv"
}

// TabList is sipTabs for the template.
func (f sipForm) TabList() []struct{ ID, Title string } { return sipTabs }

func (f *sipForm) markTabs() {
	f.TabErr = map[string]bool{}
	for k := range f.Errors {
		if t := sipFieldTab(k); t != "" {
			f.TabErr[t] = true
		}
	}
	f.Tab = "acct"
	for _, t := range sipTabs {
		if f.TabErr[t.ID] {
			f.Tab = t.ID
			break
		}
	}
}

func (s *Server) renderSIPForm(w http.ResponseWriter, r *http.Request, u *auth.User, status int, f sipForm, p page) {
	if u.IsAdmin() {
		f.Users, _ = s.Auth.ListUsers(r.Context())
	}
	if f.Rec != nil && !f.New {
		f.Status, f.HasState = s.sipStatus(f.Rec.ID)
		f.Warnings = f.Rec.Warnings()
		s.sipHistory(r, u, &f)
	}
	f.markTabs()
	title := "New VoIP line"
	if !f.New {
		title = f.Rec.Label
	}
	p.Title, p.User, p.Data = title, u, f
	s.render(w, status, "sip_account.html", p)
}

// sipHistory fills in what the page of an existing line shows besides the form: how its registration went, and its last calls.
// A failure of either leaves the section out; the form is what the page is for.
func (s *Server) sipHistory(r *http.Request, u *auth.User, f *sipForm) {
	ctx := r.Context()
	now := time.Now()
	if ev, err := sipevents.Window(ctx, s.Pool, f.Rec.ID, now, 7*24*time.Hour); err == nil {
		f.Summary = sipevents.Summarise(ev, now, 7*24*time.Hour)
	}
	f.History, _ = sipevents.Recent(ctx, s.Pool, f.Rec.ID, 30)
	acct := f.Rec.ID
	calls, err := (&calllog.Store{Pool: s.Pool}).List(ctx, calllog.Filter{Owner: &u.ID, Account: &acct, Limit: 5})
	if err != nil {
		return
	}
	f.Calls = s.rowsFor(calls, nil, map[uuid.UUID]string{acct: f.Rec.Label}, s.contactNames(ctx, u.ID, calls))
}

func (s *Server) sipNew(w http.ResponseWriter, r *http.Request, u *auth.User) {
	rec := &sipaccount.Record{Transport: "tls", Register: true, RegisterExpiry: 600, Keepalive: "auto", MediaEncryption: "optional",
		Codecs: append([]string(nil), sipaccount.DefaultCodecs...), DTMFMode: "rfc4733", CallerIDSource: "auto", TLSVerify: true, Enabled: true}
	s.renderSIPForm(w, r, u, http.StatusOK, sipForm{Rec: rec, New: true}, page{})
}

// codecChoices are the codecs the form offers, in the order they are offered.
var codecChoices = []string{"G722", "PCMA", "PCMU"}

// formRecord reads the VoIP line form into the account it asks for. The
// form always sends every field, so an unchecked box means false. base is
// the account's advanced settings as stored: what the form does not carry
// (session timers, public address ...) is kept. Some fields map onto
// columns the way the LuCI page names them: "Try to autodetect" for the
// codecs, key tones and encryption, the encryption menu, and "Registrar"
// with an optional "SIP domain" (the SIP side wants both).
func formRecord(r *http.Request, base sipaccount.Advanced) (*sipaccount.Record, sipaccount.ValidationError) {
	str := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	on := func(k string) bool { return r.PostFormValue(k) == "on" }
	bp := func(b bool) *bool { return &b }
	verr := sipaccount.ValidationError{}

	rec := &sipaccount.Record{Label: str("label"), DisplayName: str("display_name"), Username: str("username"),
		AuthUsername: str("auth_username"), OutboundProxy: str("outbound_proxy"), Transport: str("transport"),
		Register: on("register"), Keepalive: str("keepalive"), CallerIDSource: str("caller_id_source"),
		TLSVerify: on("tls_verify"), Enabled: on("enabled")}
	n, err := strconv.Atoi(str("register_expiry"))
	if err != nil {
		verr["register_expiry"] = "a number of seconds"
	}
	rec.RegisterExpiry = n

	// Registrar, SIP domain: one host is all LuCI knows. The domain is the
	// part after the @; empty, it is the registrar's host name. A registrar
	// equal to the domain is no override.
	rec.Domain, rec.Registrar = str("domain"), str("registrar")
	if rec.Domain == "" && rec.Registrar != "" {
		if host, _, err := resolve.ParseHostPort(rec.Registrar); err == nil {
			rec.Domain = host
		} else {
			rec.Domain = rec.Registrar
		}
	}
	if rec.Registrar == rec.Domain {
		rec.Registrar = ""
	}

	adv := base
	adv.UserAgent, adv.LinePrefix = str("user_agent"), str("line_prefix")
	adv.LineName, adv.Outbound2 = str("line_name"), str("outbound2")
	adv.SIPNat, adv.MediaNat = on("sipnat"), str("medianat")
	adv.StunServer, adv.LegacySTUNServer, adv.StunUser = str("stunserver"), "", str("stunuser")
	adv.RTPKeep, adv.PTime, adv.Rel100 = str("rtpkeep"), str("ptime"), str("rel100")
	adv.MWI, adv.Transfer, adv.Extra = on("mwi"), bp(on("transfer")), str("extra")
	adv.MediaAuto = bp(on("media_auto"))
	adv.Numbers = numberplan.Rules{CountryCode: str("country_code"), AreaCode: str("area_code"), TrunkPrefix: str("trunk_prefix"),
		IntlPrefix: str("intl_prefix"), StripPrefix: str("strip_prefix"), DialAs: str("dial_as"), ShowAs: str("show_as")}

	// Codecs: automatic offers them all; else the ticked ones in the order
	// shown. An older client posts a comma list in "codecs".
	switch {
	case on("media_auto"):
		rec.Codecs = append([]string(nil), sipaccount.DefaultCodecs...)
	case r.PostForm.Has("codecs"):
		for _, c := range strings.Split(r.PostFormValue("codecs"), ",") {
			if c = strings.ToUpper(strings.TrimSpace(c)); c != "" {
				rec.Codecs = append(rec.Codecs, c)
			}
		}
	default:
		picked := r.PostForm["codec"]
		for _, c := range codecChoices {
			if slices.ContainsFunc(picked, func(p string) bool { return strings.EqualFold(p, c) }) {
				rec.Codecs = append(rec.Codecs, c)
			}
		}
	}

	// Key tones: "automatic" has no column of its own.
	rec.DTMFMode = str("dtmf_mode")
	adv.DTMF = ""
	if rec.DTMFMode == "auto" {
		rec.DTMFMode, adv.DTMF = "rfc4733", "auto"
	}

	// Encryption: autodetect is SRTP when offered; else the menu, in the
	// LuCI names. An older client posts the column values in media_encryption.
	adv.EncAuto, adv.MediaEnc = bp(on("enc_auto")), ""
	choice, haveChoice := str("mediaenc"), r.PostForm.Has("mediaenc")
	switch {
	case on("enc_auto"):
		rec.MediaEncryption = "optional"
	case haveChoice:
		col, extra, ok := sipaccount.EncColumn(choice)
		if !ok {
			col = choice // not a choice: the store says so
		}
		rec.MediaEncryption, adv.MediaEnc = col, extra
	default:
		rec.MediaEncryption = str("media_encryption")
	}
	rec.Advanced = adv
	return rec, verr
}

// formInput reads the VoIP line form for the store.
func formInput(r *http.Request, u *auth.User, base sipaccount.Advanced) (sipaccount.Input, *sipaccount.Record, error) {
	rec, verr := formRecord(r, base)
	in := sipaccount.Input{Label: &rec.Label, DisplayName: &rec.DisplayName, Username: &rec.Username, Domain: &rec.Domain,
		AuthUsername: &rec.AuthUsername, Registrar: &rec.Registrar, OutboundProxy: &rec.OutboundProxy,
		Transport: &rec.Transport, Register: &rec.Register, Keepalive: &rec.Keepalive,
		MediaEncryption: &rec.MediaEncryption, DTMFMode: &rec.DTMFMode, CallerIDSource: &rec.CallerIDSource,
		TLSVerify: &rec.TLSVerify, Enabled: &rec.Enabled, RegisterExpiry: &rec.RegisterExpiry,
		Codecs: &rec.Codecs, Advanced: &rec.Advanced}
	if len(verr) > 0 {
		return in, rec, verr
	}
	if pw := r.PostFormValue("password"); pw != "" {
		in.Password = &pw
	} else if r.PostFormValue("clear_password") == "on" {
		empty := ""
		in.Password = &empty
	}
	if pw := r.PostFormValue("stunpass"); pw != "" {
		in.StunPass = &pw
	} else if r.PostFormValue("clear_stunpass") == "on" {
		empty := ""
		in.StunPass = &empty
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
				return in, rec, sipaccount.ValidationError{"owner": "no such user"}
			}
			p := &id
			in.OwnerID = &p
		}
	}
	return in, rec, nil
}

func (s *Server) sipCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	in, shown, err := formInput(r, u, sipaccount.Advanced{})
	if err == nil {
		var rec *sipaccount.Record
		if rec, err = s.SIPAccounts.Create(r.Context(), u, in); err == nil {
			http.Redirect(w, r, "/sip-accounts/"+rec.ID.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: a UUID we generated
			return
		}
	}
	s.sipFormError(w, r, u, err, sipForm{Rec: shown, New: true, Owner: r.PostFormValue("owner")})
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
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	rec, ok := s.sipByPath(w, r, u)
	if !ok {
		return
	}
	in, shown, err := formInput(r, u, rec.Advanced)
	if err == nil {
		if _, err = s.SIPAccounts.Update(r.Context(), u, rec.ID, in); err == nil {
			http.Redirect(w, r, "/sip-accounts/"+rec.ID.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: a UUID we generated
			return
		}
	}
	shown.ID, shown.HasPassword, shown.HasStunPass = rec.ID, rec.HasPassword, rec.HasStunPass
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
