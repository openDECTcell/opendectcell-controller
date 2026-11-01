// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sipaccount keeps SIP accounts in the database: the password
// sealed (docs/threat-model.md §4.2), the owner-or-admin rule on every
// access (AGENTS.md §5), an audit entry for every change. It hands the
// running SIP side its accounts with the passwords opened.
package sipaccount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

var (
	ErrNotFound = errors.New("sipaccount: not found")
	ErrInUse    = errors.New("sipaccount: the account is used by a handset line")
)

// ValidationError lists problems by field, for forms and the API.
type ValidationError map[string]string

func (v ValidationError) Error() string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + v[k]
	}
	return "invalid account: " + strings.Join(parts, "; ")
}

// Record is an account as shown to users. It never holds the password.
type Record struct {
	ID              uuid.UUID
	OwnerID         *uuid.UUID
	Label           string
	DisplayName     string
	Username        string
	Domain          string
	AuthUsername    string
	HasPassword     bool
	HasStunPass     bool // a TURN password is stored (never returned)
	Registrar       string
	OutboundProxy   string
	Transport       string
	Register        bool
	RegisterExpiry  int
	Keepalive       string
	MediaEncryption string
	Codecs          []string
	DTMFMode        string
	CallerIDSource  string
	TLSVerify       bool
	Advanced        Advanced
	Enabled         bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Advanced holds the parameters that have no column of their own
// (docs/api/openapi.yaml, SipAdvanced). The SIP side uses user_agent only;
// the rest is stored, and the VoIP profile page shows it.
//
// The fields from line_name down carry the option names of the VoIP
// profiles in luci-app-opendectcell (the maintainers' notes, 2026-11-01), so a profile
// reads the same on both sides. Options that default to on there are
// pointers: nil means "the default", false is a choice.
type Advanced struct {
	UserAgent           string `json:"user_agent,omitempty"`
	SessionTimers       bool   `json:"session_timers,omitempty"`
	SessionExpires      int    `json:"session_expires,omitempty"`
	IdentityHeader      string `json:"identity_header,omitempty"`
	LegacySTUNServer    string `json:"stun_server,omitempty"` // before stunserver; shown as its fallback
	PublicAddress       string `json:"public_address,omitempty"`
	LocalPort           int    `json:"local_port,omitempty"`
	InterDigitTimeoutMs int    `json:"inter_digit_timeout_ms,omitempty"`
	LinePrefix          string `json:"line_prefix,omitempty"`
	PRACK               bool   `json:"prack,omitempty"`

	LineName   string `json:"line_name,omitempty"`  // what a CAT-iq handset shows for the line, at most 15 octets
	Outbound2  string `json:"outbound2,omitempty"`  // second outbound proxy
	SIPNat     bool   `json:"sipnat,omitempty"`     // SIP outbound (RFC 5626)
	MediaNat   string `json:"medianat,omitempty"`   // "", stun, turn, ice
	StunServer string `json:"stunserver,omitempty"` // STUN/TURN server
	StunUser   string `json:"stunuser,omitempty"`   // TURN user (the password is the column stunpass_enc)
	RTPKeep    string `json:"rtpkeep,omitempty"`    // "", zero, stun, dyna, rtcp
	MediaAuto  *bool  `json:"media_auto,omitempty"` // codecs and key tones by autodetection
	PTime      string `json:"ptime,omitempty"`      // 10, 20, 30, 40
	DTMF       string `json:"dtmf,omitempty"`       // "auto" only: the other modes are the column dtmf_mode
	Rel100     string `json:"rel100,omitempty"`     // yes, no, required
	MWI        bool   `json:"mwi,omitempty"`
	Transfer   *bool  `json:"transfer,omitempty"` // default on
	EncAuto    *bool  `json:"enc_auto,omitempty"` // encryption by autodetection: the column media_encryption "optional"
	MediaEnc   string `json:"mediaenc,omitempty"` // "srtp-mandf" or "dtls_srtp" only: the rest is the column media_encryption
	Extra      string `json:"extra,omitempty"`    // further account parameters, baresip syntax
}

// DefaultCodecs is what "Try to autodetect" offers, HD first.
var DefaultCodecs = []string{"G722", "PCMA", "PCMU"}

// TransferOn reports whether call transfer is allowed (default on).
func (a Advanced) TransferOn() bool { return a.Transfer == nil || *a.Transfer }

// MediaAutoOn reports whether the profile takes codecs and key tones by
// autodetection. An account that never said, has it when its codecs are the
// default list and its key tones RTP events.
func (r *Record) MediaAutoOn() bool {
	if r.Advanced.MediaAuto != nil {
		return *r.Advanced.MediaAuto
	}
	return strings.Join(r.Codecs, ",") == strings.Join(DefaultCodecs, ",") && r.DTMFMode == "rfc4733"
}

// EncAutoOn reports whether the profile takes media encryption by
// autodetection: SRTP when the other side offers it, else plain speech.
func (r *Record) EncAutoOn() bool {
	return r.MediaEncryption == "optional" && (r.Advanced.EncAuto == nil || *r.Advanced.EncAuto)
}

// EncChoice is the media encryption as the VoIP profile page names it:
// "" (none), srtp, srtp-mand, srtp-mandf or dtls_srtp. Only the first
// three have their own column value; the other two ride on "mandatory"
// (the stack does SDES-SRTP only: DTLS-SRTP is stored, not spoken).
func (r *Record) EncChoice() string {
	switch r.MediaEncryption {
	case "optional":
		return "srtp"
	case "mandatory":
		if r.Advanced.MediaEnc == "srtp-mandf" || r.Advanced.MediaEnc == "dtls_srtp" {
			return r.Advanced.MediaEnc
		}
		return "srtp-mand"
	}
	return ""
}

// EncColumn maps a page choice to the column value and the extra the
// advanced field keeps; ok is false for a choice that does not exist.
func EncColumn(choice string) (column, extra string, ok bool) {
	switch choice {
	case "":
		return "off", "", true
	case "srtp":
		return "optional", "", true
	case "srtp-mand":
		return "mandatory", "", true
	case "srtp-mandf", "dtls_srtp":
		return "mandatory", choice, true
	}
	return "", "", false
}

// DTMFChoice is the key-tone mode as the page names it: rfc4733, info,
// auto or inband (inband has no counterpart there).
func (r *Record) DTMFChoice() string {
	if r.DTMFMode == "rfc4733" && r.Advanced.DTMF == "auto" {
		return "auto"
	}
	return r.DTMFMode
}

// StunServerShown is the STUN/TURN server, or the one an older API client
// stored as stun_server.
func (a Advanced) StunServerShown() string {
	if a.StunServer != "" {
		return a.StunServer
	}
	return a.LegacySTUNServer
}

// Warnings are problems that are allowed but unwise.
func (r *Record) Warnings() []string {
	w := toSIP(r, "").Warnings()
	if r.Advanced.MediaEnc == "dtls_srtp" && r.MediaEncryption == "mandatory" {
		w = append(w, "dtls_srtp_unsupported")
	}
	return w
}

// Input is a create or update. Nil fields are left unchanged on update;
// on create they take the schema defaults. Password "" clears it; nil keeps.
type Input struct {
	OwnerID         **uuid.UUID // admin only; *OwnerID nil = shared
	Label           *string
	DisplayName     *string
	Username        *string
	Domain          *string
	AuthUsername    *string
	Password        *string
	StunPass        *string // the TURN password; same rules as Password
	Registrar       *string
	OutboundProxy   *string
	Transport       *string
	Register        *bool
	RegisterExpiry  *int
	Keepalive       *string
	MediaEncryption *string
	Codecs          *[]string
	DTMFMode        *string
	CallerIDSource  *string
	TLSVerify       *bool
	Advanced        *Advanced
	Enabled         *bool
}

// Store holds SIP accounts.
type Store struct {
	Pool    *pgxpool.Pool
	Keyring *secret.Keyring
	// OnChange, if set, runs after every committed change, e.g. to
	// reconcile the running user agents.
	OnChange func()
}

func (s *Store) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

const cols = `id, owner_user_id, label, display_name, username, domain, coalesce(auth_username, ''), password_enc IS NOT NULL,
	coalesce(registrar, ''), coalesce(outbound_proxy, ''), transport, register, register_expiry, keepalive, media_encryption,
	codecs, dtmf_mode, caller_id_source, tls_verify, advanced, enabled, created_at, updated_at, stunpass_enc IS NOT NULL`

func scan(r pgx.Row) (*Record, error) {
	var a Record
	var adv []byte
	err := r.Scan(&a.ID, &a.OwnerID, &a.Label, &a.DisplayName, &a.Username, &a.Domain, &a.AuthUsername, &a.HasPassword,
		&a.Registrar, &a.OutboundProxy, &a.Transport, &a.Register, &a.RegisterExpiry, &a.Keepalive, &a.MediaEncryption,
		&a.Codecs, &a.DTMFMode, &a.CallerIDSource, &a.TLSVerify, &adv, &a.Enabled, &a.CreatedAt, &a.UpdatedAt, &a.HasStunPass)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(adv, &a.Advanced); err != nil {
		return nil, err
	}
	return &a, nil
}

// mayUse is the rule for who may see and change an account: its owner only,
// and the shared ones (no owner) the admins manage. An admin does not see
// another user's lines: they hold that user's credentials.
func mayUse(u *auth.User, owner *uuid.UUID) bool {
	if u == nil || u.Disabled {
		return false
	}
	if owner == nil {
		return u.IsAdmin()
	}
	return *owner == u.ID
}

// List returns the accounts u may see: their own, and for admins the shared
// ones too (owner filters, for admins: "shared", or their own id; another user's
// id lists nothing).
func (s *Store) List(ctx context.Context, u *auth.User, owner string) ([]*Record, error) {
	if u == nil {
		return nil, auth.ErrForbidden
	}
	q := "SELECT " + cols + " FROM sip_account WHERE "
	var args []any
	switch {
	case !u.IsAdmin():
		q += "owner_user_id = $1"
		args = append(args, u.ID)
	case owner == "shared":
		q += "owner_user_id IS NULL"
	case owner != "":
		id, err := uuid.Parse(owner)
		if err != nil {
			return nil, ValidationError{"owner": "must be a user id or 'shared'"}
		}
		if id != u.ID {
			return nil, nil // another user's lines are not the admin's to list
		}
		q += "owner_user_id = $1"
		args = append(args, id)
	default:
		q += "(owner_user_id = $1 OR owner_user_id IS NULL)"
		args = append(args, u.ID)
	}
	rows, err := s.Pool.Query(ctx, q+" ORDER BY lower(label), id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one account; ErrNotFound if it doesn't exist or u may not
// see it.
func (s *Store) Get(ctx context.Context, u *auth.User, id uuid.UUID) (*Record, error) {
	r, err := scan(s.Pool.QueryRow(ctx, "SELECT "+cols+" FROM sip_account WHERE id = $1", id))
	if err != nil {
		return nil, err
	}
	if !mayUse(u, r.OwnerID) {
		return nil, ErrNotFound
	}
	return r, nil
}

// LineUse is a handset line an account serves.
type LineUse struct {
	HandsetID uuid.UUID
	Handset   string // the handset's name, or its model
	Line      int    // 1 or 2
}

// Uses returns, per account, the lines of u's own handsets that have it. A
// handset's lines are private to its owner (internal/handset), so another
// user's handsets are not listed even on a shared account.
func (s *Store) Uses(ctx context.Context, u *auth.User, ids []uuid.UUID) (map[uuid.UUID][]LineUse, error) {
	out := map[uuid.UUID][]LineUse{}
	if u == nil || len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT hl.sip_account_id, h.id, coalesce(nullif(h.display_name, ''), nullif(h.model, ''), 'Handset'), hl.line
		FROM handset_line hl JOIN handset h ON h.id = hl.handset_id
		WHERE hl.sip_account_id = ANY($1) AND h.owner_user_id = $2
		ORDER BY lower(coalesce(nullif(h.display_name, ''), nullif(h.model, ''), 'Handset')), hl.line`, ids, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var acct uuid.UUID
		var l LineUse
		if err := rows.Scan(&acct, &l.HandsetID, &l.Handset, &l.Line); err != nil {
			return nil, err
		}
		out[acct] = append(out[acct], l)
	}
	return out, rows.Err()
}

func loc(id uuid.UUID) secret.Location { return locCol(id, "password_enc") }

func locCol(id uuid.UUID, col string) secret.Location {
	return secret.Location{Table: "sip_account", Column: col, RowID: id.String()}
}

func defaults() *Record {
	return &Record{Transport: "tls", Register: true, RegisterExpiry: 600, Keepalive: "auto", MediaEncryption: "optional",
		Codecs: []string{"G722", "PCMA", "PCMU"}, DTMFMode: "rfc4733", CallerIDSource: "auto", TLSVerify: true, Enabled: true}
}

// apply merges in into r and reports the fields it changed.
func apply(r *Record, in Input) []string {
	var changed []string
	set := func(name string, dst *string, v *string) {
		if v != nil && *v != *dst {
			*dst = strings.TrimSpace(*v)
			changed = append(changed, name)
		}
	}
	set("label", &r.Label, in.Label)
	set("display_name", &r.DisplayName, in.DisplayName)
	set("username", &r.Username, in.Username)
	set("domain", &r.Domain, in.Domain)
	set("auth_username", &r.AuthUsername, in.AuthUsername)
	set("registrar", &r.Registrar, in.Registrar)
	set("outbound_proxy", &r.OutboundProxy, in.OutboundProxy)
	set("transport", &r.Transport, in.Transport)
	set("keepalive", &r.Keepalive, in.Keepalive)
	set("media_encryption", &r.MediaEncryption, in.MediaEncryption)
	set("dtmf_mode", &r.DTMFMode, in.DTMFMode)
	set("caller_id_source", &r.CallerIDSource, in.CallerIDSource)
	setB := func(name string, dst *bool, v *bool) {
		if v != nil && *v != *dst {
			*dst = *v
			changed = append(changed, name)
		}
	}
	setB("register", &r.Register, in.Register)
	setB("tls_verify", &r.TLSVerify, in.TLSVerify)
	setB("enabled", &r.Enabled, in.Enabled)
	if in.RegisterExpiry != nil && *in.RegisterExpiry != r.RegisterExpiry {
		r.RegisterExpiry = *in.RegisterExpiry
		changed = append(changed, "register_expiry")
	}
	if in.Codecs != nil && strings.Join(*in.Codecs, ",") != strings.Join(r.Codecs, ",") {
		r.Codecs = append([]string(nil), *in.Codecs...)
		changed = append(changed, "codecs")
	}
	if in.Advanced != nil && !reflect.DeepEqual(*in.Advanced, r.Advanced) {
		r.Advanced = *in.Advanced
		changed = append(changed, "advanced")
	}
	if in.OwnerID != nil {
		nw, old := *in.OwnerID, r.OwnerID
		if (nw == nil) != (old == nil) || (nw != nil && *nw != *old) {
			r.OwnerID = nw
			changed = append(changed, "owner")
		}
	}
	return changed
}

func validate(r *Record) error {
	v := ValidationError{}
	if r.Label == "" || len(r.Label) > 64 {
		v["label"] = "1 to 64 characters"
	}
	for k, msg := range map[string]bool{"keepalive": oneOf(r.Keepalive, "auto", "none", "crlf", "options"),
		"media_encryption": oneOf(r.MediaEncryption, "off", "optional", "mandatory"),
		"dtmf_mode":        oneOf(r.DTMFMode, "rfc4733", "info", "inband"),
		"caller_id_source": oneOf(r.CallerIDSource, "auto", "from", "pai", "rpid")} {
		if !msg {
			v[k] = "not a valid choice"
		}
	}
	if r.Advanced.LinePrefix != "" && strings.Trim(r.Advanced.LinePrefix, "*#0123456789") != "" {
		v["advanced.line_prefix"] = "digits, * and # only"
	}
	if len(r.Advanced.UserAgent) > 128 {
		v["advanced.user_agent"] = "at most 128 characters"
	}
	validateProfile(v, &r.Advanced)
	if err := toSIP(r, "").Validate(); err != nil {
		v["account"] = err.Error()
	}
	if len(v) > 0 {
		return v
	}
	return nil
}

// validateProfile checks the options of the VoIP profile page the way the
// LuCI page does.
func validateProfile(v ValidationError, a *Advanced) {
	if len(a.LineName) > 15 { // octets, as the handset's name field counts them
		v["advanced.line_name"] = "at most 15 characters (fewer with umlauts)"
	}
	for k, hp := range map[string]string{"advanced.outbound2": a.Outbound2, "advanced.stunserver": a.StunServer,
		"advanced.stun_server": a.LegacySTUNServer} {
		if hp == "" {
			continue
		}
		if _, _, err := resolve.ParseHostPort(hp); err != nil || strings.ContainsAny(hp, " \t\r\n<>@;\"") {
			v[k] = "a host name or address, optionally with :port"
		}
	}
	for k, c := range map[string]struct {
		val  string
		opts []string
	}{
		"advanced.medianat": {a.MediaNat, []string{"", "stun", "turn", "ice"}},
		"advanced.rtpkeep":  {a.RTPKeep, []string{"", "zero", "stun", "dyna", "rtcp"}},
		"advanced.ptime":    {a.PTime, []string{"", "10", "20", "30", "40"}},
		"advanced.dtmf":     {a.DTMF, []string{"", "auto"}},
		"advanced.rel100":   {a.Rel100, []string{"", "yes", "no", "required"}},
		"advanced.mediaenc": {a.MediaEnc, []string{"", "srtp-mandf", "dtls_srtp"}},
	} {
		if !oneOf(c.val, c.opts...) {
			v[k] = "not a valid choice"
		}
	}
	if len(a.StunUser) > 128 {
		v["advanced.stunuser"] = "at most 128 characters"
	}
	if a.Extra != "" && !extraRE.MatchString(a.Extra) {
		v["advanced.extra"] = ";key=value pairs, no spaces"
	}
}

var extraRE = regexp.MustCompile(`^(;[a-z0-9_]+=[^;\s"<>]+)+$`)

func oneOf(s string, opts ...string) bool {
	for _, o := range opts {
		if s == o {
			return true
		}
	}
	return false
}

// Create adds an account owned by u (admins may choose any owner or
// shared).
func (s *Store) Create(ctx context.Context, u *auth.User, in Input) (*Record, error) {
	if u == nil {
		return nil, auth.ErrForbidden
	}
	r := defaults()
	r.OwnerID = &u.ID
	if in.OwnerID != nil && !u.IsAdmin() {
		return nil, auth.ErrForbidden
	}
	apply(r, in)
	if err := validate(r); err != nil {
		return nil, err
	}
	if err := s.checkOwner(ctx, r.OwnerID); err != nil {
		return nil, err
	}
	adv, _ := json.Marshal(r.Advanced)
	var out *Record
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `INSERT INTO sip_account (owner_user_id, label, display_name, username, domain, auth_username,
			registrar, outbound_proxy, transport, register, register_expiry, keepalive, media_encryption, codecs, dtmf_mode,
			caller_id_source, tls_verify, advanced, enabled)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), nullif($8, ''), $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
			RETURNING id`,
			r.OwnerID, r.Label, r.DisplayName, r.Username, r.Domain, r.AuthUsername, r.Registrar, r.OutboundProxy, r.Transport,
			r.Register, r.RegisterExpiry, r.Keepalive, r.MediaEncryption, r.Codecs, r.DTMFMode, r.CallerIDSource, r.TLSVerify, adv, r.Enabled).Scan(&id)
		if err != nil {
			return err
		}
		if in.Password != nil && *in.Password != "" {
			if err := s.setPassword(ctx, tx, id, *in.Password); err != nil {
				return err
			}
		}
		if in.StunPass != nil && *in.StunPass != "" {
			if err := s.setSecret(ctx, tx, id, "stunpass_enc", "stunpass", *in.StunPass); err != nil {
				return err
			}
		}
		if out, err = scan(tx.QueryRow(ctx, "SELECT "+cols+" FROM sip_account WHERE id = $1", id)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "sip_account.create", ObjectType: "sip_account", ObjectID: id.String(),
			Details: map[string]any{"label": r.Label, "aor": r.Username + "@" + r.Domain, "owner": ownerStr(r.OwnerID)}})
	})
	if err != nil {
		return nil, err
	}
	s.changed()
	return out, nil
}

func ownerStr(o *uuid.UUID) string {
	if o == nil {
		return "shared"
	}
	return o.String()
}

func (s *Store) checkOwner(ctx context.Context, owner *uuid.UUID) error {
	if owner == nil {
		return nil
	}
	var ok bool
	if err := s.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM app_user WHERE id = $1)", *owner).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ValidationError{"owner": "no such user"}
	}
	return nil
}

func (s *Store) setPassword(ctx context.Context, tx pgx.Tx, id uuid.UUID, pw string) error {
	return s.setSecret(ctx, tx, id, "password_enc", "password", pw)
}

// setSecret seals pw into one of the account's encrypted columns; "" clears it.
func (s *Store) setSecret(ctx context.Context, tx pgx.Tx, id uuid.UUID, col, field, pw string) error {
	if len(pw) > 256 {
		return ValidationError{field: "at most 256 characters"}
	}
	var enc []byte
	if pw != "" {
		var err error
		if enc, err = s.Keyring.Seal(locCol(id, col), []byte(pw)); err != nil {
			return err
		}
	}
	// col is one of two constants above, never input
	_, err := tx.Exec(ctx, "UPDATE sip_account SET "+col+" = $2 WHERE id = $1", id, enc) //nolint:gosec // G202: col is a constant
	return err
}

// Update changes an account u may access. Changing the owner is admin-only.
func (s *Store) Update(ctx context.Context, u *auth.User, id uuid.UUID, in Input) (*Record, error) {
	if in.OwnerID != nil && !u.IsAdmin() {
		return nil, auth.ErrForbidden
	}
	var out *Record
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		r, err := scan(tx.QueryRow(ctx, "SELECT "+cols+" FROM sip_account WHERE id = $1 FOR UPDATE", id))
		if err != nil {
			return err
		}
		if !mayUse(u, r.OwnerID) {
			return ErrNotFound
		}
		changed := apply(r, in)
		if in.Password != nil {
			changed = append(changed, "password")
		}
		if in.StunPass != nil {
			changed = append(changed, "stunpass")
		}
		if len(changed) == 0 {
			out = r
			return nil
		}
		if err := validate(r); err != nil {
			return err
		}
		if err := s.checkOwner(ctx, r.OwnerID); err != nil {
			return err
		}
		adv, _ := json.Marshal(r.Advanced)
		if _, err := tx.Exec(ctx, `UPDATE sip_account SET owner_user_id = $2, label = $3, display_name = $4, username = $5, domain = $6,
			auth_username = nullif($7, ''), registrar = nullif($8, ''), outbound_proxy = nullif($9, ''), transport = $10, register = $11,
			register_expiry = $12, keepalive = $13, media_encryption = $14, codecs = $15, dtmf_mode = $16, caller_id_source = $17,
			tls_verify = $18, advanced = $19, enabled = $20, updated_at = now() WHERE id = $1`,
			id, r.OwnerID, r.Label, r.DisplayName, r.Username, r.Domain, r.AuthUsername, r.Registrar, r.OutboundProxy, r.Transport,
			r.Register, r.RegisterExpiry, r.Keepalive, r.MediaEncryption, r.Codecs, r.DTMFMode, r.CallerIDSource, r.TLSVerify, adv, r.Enabled); err != nil {
			return err
		}
		if in.Password != nil {
			if err := s.setPassword(ctx, tx, id, *in.Password); err != nil {
				return err
			}
		}
		if in.StunPass != nil {
			if err := s.setSecret(ctx, tx, id, "stunpass_enc", "stunpass", *in.StunPass); err != nil {
				return err
			}
		}
		// The owner rule for lines (schema.md §3): if the owner changed,
		// lines on handsets the new owner doesn't own lose this account.
		if containsStr(changed, "owner") {
			if _, err := tx.Exec(ctx, `DELETE FROM handset_line hl USING handset h
				WHERE hl.handset_id = h.id AND hl.sip_account_id = $1
				  AND $2::uuid IS NOT NULL AND h.owner_user_id IS DISTINCT FROM $2`, id, r.OwnerID); err != nil {
				return err
			}
		}
		if out, err = scan(tx.QueryRow(ctx, "SELECT "+cols+" FROM sip_account WHERE id = $1", id)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "sip_account.update", ObjectType: "sip_account", ObjectID: id.String(),
			Details: map[string]any{"fields": changed}})
	})
	if err != nil {
		return nil, err
	}
	s.changed()
	return out, nil
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Delete removes an account u may access, unless a handset line uses it.
func (s *Store) Delete(ctx context.Context, u *auth.User, id uuid.UUID) error {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var owner *uuid.UUID
		var label string
		err := tx.QueryRow(ctx, "SELECT owner_user_id, label FROM sip_account WHERE id = $1 FOR UPDATE", id).Scan(&owner, &label)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !mayUse(u, owner) {
			return ErrNotFound
		}
		var used bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM handset_line WHERE sip_account_id = $1)", id).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrInUse
		}
		if _, err := tx.Exec(ctx, "DELETE FROM sip_account WHERE id = $1", id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "sip_account.delete", ObjectType: "sip_account", ObjectID: id.String(),
			Details: map[string]any{"label": label}})
	})
	if err != nil {
		return err
	}
	s.changed()
	return nil
}

func toSIP(r *Record, password string) sip.Account {
	return sip.Account{ID: r.ID, Label: r.Label, DisplayName: r.DisplayName, Username: r.Username, Domain: r.Domain,
		AuthUsername: r.AuthUsername, Password: password, Registrar: r.Registrar, OutboundProxy: r.OutboundProxy,
		Transport: resolve.Transport(r.Transport), Register: r.Register, RegisterExpiry: r.RegisterExpiry, Keepalive: r.Keepalive,
		Encryption: sdp.Encryption(r.MediaEncryption), Codecs: r.Codecs, DTMFMode: r.DTMFMode, CallerIDSource: r.CallerIDSource,
		TLSVerify: r.TLSVerify, Enabled: r.Enabled, UserAgent: r.Advanced.UserAgent}
}

// RuntimeAccounts returns every account for the SIP side, passwords
// opened. An account whose password cannot be opened is skipped and
// reported, never started without it.
func (s *Store) RuntimeAccounts(ctx context.Context) ([]sip.Account, error) {
	rows, err := s.Pool.Query(ctx, "SELECT "+cols+", password_enc FROM sip_account ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sip.Account
	var errs []error
	for rows.Next() {
		var enc []byte
		var a Record
		var adv []byte
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.Label, &a.DisplayName, &a.Username, &a.Domain, &a.AuthUsername, &a.HasPassword,
			&a.Registrar, &a.OutboundProxy, &a.Transport, &a.Register, &a.RegisterExpiry, &a.Keepalive, &a.MediaEncryption,
			&a.Codecs, &a.DTMFMode, &a.CallerIDSource, &a.TLSVerify, &adv, &a.Enabled, &a.CreatedAt, &a.UpdatedAt, &a.HasStunPass, &enc); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(adv, &a.Advanced)
		pw := ""
		if enc != nil {
			p, err := s.Keyring.Open(loc(a.ID), enc)
			if err != nil {
				errs = append(errs, fmt.Errorf("account %s: password: %w", a.ID, err))
				continue
			}
			pw = string(p)
		}
		out = append(out, toSIP(&a, pw))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, errors.Join(errs...)
}
