// SPDX-License-Identifier: AGPL-3.0-or-later

package transfer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/core"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/profiles"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
)

// Records is the DECT core's part of a transfer: a handset's key out, a handset with its key in.
type Records interface {
	ExportKey(ctx context.Context, owner, id uuid.UUID) (core.Key, error)
	ImportHandset(ctx context.Context, owner uuid.UUID, in core.Imported) (uuid.UUID, error)
}

// Store exports and imports a user's setup through the stores that own each part.
type Store struct {
	Pool     *pgxpool.Pool
	Records  Records
	Lines    *sipaccount.Store
	Contacts *contacts.Store
	Handsets *handset.Store
}

// Options say what an export holds. Secrets adds the keys of the handsets and the passwords of the lines; the file must then be
// encrypted (Seal).
type Options struct {
	Handsets, Lines, Contacts, Secrets bool
}

// Export collects the user's own setup. Nothing is audited here: the caller records the export once the file was handed over.
func (s *Store) Export(ctx context.Context, u *auth.User, o Options) (Setup, error) {
	out := Setup{Created: time.Now().UTC().Truncate(time.Second)}
	refs := map[uuid.UUID]string{}
	if o.Lines || o.Handsets {
		recs, err := s.Lines.List(ctx, u, u.ID.String())
		if err != nil {
			return Setup{}, err
		}
		for i, r := range recs {
			if r.OwnerID == nil || *r.OwnerID != u.ID {
				continue
			}
			refs[r.ID] = "l" + strconv.Itoa(i+1)
			if !o.Lines {
				continue
			}
			l := Line{Ref: refs[r.ID], Label: r.Label, DisplayName: r.DisplayName, Username: r.Username, Domain: r.Domain,
				AuthUsername: r.AuthUsername, Registrar: r.Registrar, OutboundProxy: r.OutboundProxy, Transport: r.Transport,
				Register: r.Register, RegisterExpiry: r.RegisterExpiry, Keepalive: r.Keepalive, MediaEncryption: r.MediaEncryption,
				Codecs: r.Codecs, DTMFMode: r.DTMFMode, CallerIDSource: r.CallerIDSource, TLSVerify: r.TLSVerify, Enabled: r.Enabled,
				Advanced: r.Advanced}
			if o.Secrets {
				if l.Password, l.StunPassword, err = s.Lines.Secrets(ctx, u, r.ID); err != nil {
					return Setup{}, fmt.Errorf("line %q: %w", r.Label, err)
				}
			}
			out.Lines = append(out.Lines, l)
		}
	}
	if o.Handsets {
		hs, err := s.handsets(ctx, u, refs, o.Secrets)
		if err != nil {
			return Setup{}, err
		}
		out.Handsets = hs
	}
	if o.Contacts {
		own, err := s.ownContacts(ctx, u)
		if err != nil {
			return Setup{}, err
		}
		var b bytes.Buffer
		if err := contacts.WriteVCards(&b, own); err != nil {
			return Setup{}, err
		}
		out.Contacts = b.String()
	}
	return out, nil
}

func (s *Store) handsets(ctx context.Context, u *auth.User, refs map[uuid.UUID]string, secrets bool) ([]Handset, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, display_name, coalesce(model, ''), coalesce(profile, ''), ipui, manufacturer_emc, default_line,
			dual_ring_policy, internal_number FROM handset WHERE owner_user_id = $1 ORDER BY registered_at, id`, u.ID)
	if err != nil {
		return nil, err
	}
	type row struct {
		id uuid.UUID
		h  Handset
	}
	var list []row
	for rows.Next() {
		var r row
		var ipui []byte
		var dl, in *int16
		var emc *int32
		if err := rows.Scan(&r.id, &r.h.Name, &r.h.Model, &r.h.Profile, &ipui, &emc, &dl, &r.h.DualRing, &in); err != nil {
			rows.Close()
			return nil, err
		}
		r.h.IPUI = hex.EncodeToString(ipui)
		r.h.EMC, r.h.DefaultLine, r.h.InternalNumber = widen(emc), widen(dl), widen(in)
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Handset
	for _, r := range list {
		lr, err := s.Pool.Query(ctx, "SELECT line, sip_account_id FROM handset_line WHERE handset_id = $1 ORDER BY line", r.id)
		if err != nil {
			return nil, err
		}
		for lr.Next() {
			var line int
			var acct uuid.UUID
			if err := lr.Scan(&line, &acct); err != nil {
				lr.Close()
				return nil, err
			}
			if ref, ok := refs[acct]; ok { // a shared line is not in the file: the handset keeps no line there
				if r.h.Lines == nil {
					r.h.Lines = map[string]string{}
				}
				r.h.Lines[strconv.Itoa(line)] = ref
			}
		}
		lr.Close()
		if err := lr.Err(); err != nil {
			return nil, err
		}
		if secrets {
			k, err := s.Records.ExportKey(ctx, u.ID, r.id)
			if err != nil {
				return nil, fmt.Errorf("handset %q: key: %w", r.h.Name, err)
			}
			r.h.Key = &Key{UAK: hex.EncodeToString(k.UAK[:]), CCPortID: hex.EncodeToString(k.CCPortID), CCFixedID: hex.EncodeToString(k.CCFixedID)}
			clear(k.UAK[:])
		}
		out = append(out, r.h)
	}
	return out, nil
}

func widen[T int16 | int32](p *T) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

// Item is what an import did, or would do, with one thing of the file.
type Item struct {
	Kind   string // handset, line, contacts
	Name   string
	Action string // add, update, skip, fail
	Note   string
}

// Report is the outcome of an import. Nothing in it is a value from the secret parts of the file.
type Report struct {
	DryRun                          bool
	Items                           []Item
	Added, Updated, Skipped, Failed int
	ChangedHandsets                 []uuid.UUID // for the DECT side to follow (names, lines)
}

func (r *Report) add(kind, name, action, note string) {
	r.Items = append(r.Items, Item{kind, name, action, note})
	switch action {
	case "add":
		r.Added++
	case "update":
		r.Updated++
	case "skip":
		r.Skipped++
	case "fail":
		r.Failed++
	}
}

// Import brings the setup into the account of u. dry: nothing is written, the report says what would happen. Every item stands on
// its own (a bad one is reported and the others go on). Lines the user has already (same user name and domain) are kept as they
// are; a handset the user has already gets the settings of the file, its key stays; a handset of another user, or any other whose
// identity is known here, is never touched.
func (s *Store) Import(ctx context.Context, u *auth.User, set Setup, dry bool) (Report, error) {
	rep := Report{DryRun: dry}
	if len(set.Handsets) > MaxHandsets || len(set.Lines) > MaxLines {
		return rep, fmt.Errorf("transfer: the file holds more than %d handsets or %d lines", MaxHandsets, MaxLines)
	}
	byRef, err := s.importLines(ctx, u, set.Lines, dry, &rep)
	if err != nil {
		return rep, err
	}
	for _, h := range set.Handsets {
		if err := s.importHandset(ctx, u, h, byRef, dry, &rep); err != nil {
			return rep, err
		}
	}
	if strings.TrimSpace(set.Contacts) != "" {
		if err := s.importContacts(ctx, u, set.Contacts, dry, &rep); err != nil {
			return rep, err
		}
	}
	if !dry {
		_ = audit.Record(ctx, s.Pool, audit.Entry{Action: "setup.import", ObjectType: "user", ObjectID: u.ID.String(),
			Details: map[string]any{"added": rep.Added, "updated": rep.Updated, "skipped": rep.Skipped, "failed": rep.Failed}})
	}
	return rep, nil
}

func given(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func aor(user, domain string) string { return strings.ToLower(user + "@" + domain) }

// importLines returns the account of each Ref of the file (uuid.Nil for one a dry run would make).
func (s *Store) importLines(ctx context.Context, u *auth.User, lines []Line, dry bool, rep *Report) (map[string]uuid.UUID, error) {
	byRef := map[string]uuid.UUID{}
	if len(lines) == 0 {
		return byRef, nil
	}
	have, err := s.Lines.List(ctx, u, u.ID.String())
	if err != nil {
		return nil, err
	}
	known := map[string]uuid.UUID{}
	own := 0
	for _, r := range have {
		if r.OwnerID != nil && *r.OwnerID == u.ID {
			known[aor(r.Username, r.Domain)] = r.ID
			own++
		}
	}
	for _, l := range lines {
		name := l.Label
		if id, ok := known[aor(l.Username, l.Domain)]; ok {
			byRef[l.Ref] = id
			rep.add("line", name, "skip", "you have this line already; it is left as it is")
			continue
		}
		if !u.IsAdmin() && own >= sipaccount.MaxAccountsPerUser {
			rep.add("line", name, "fail", fmt.Sprintf("at most %d lines per user", sipaccount.MaxAccountsPerUser))
			continue
		}
		// a field the file leaves out takes the default of a new line
		in := sipaccount.Input{Label: &l.Label, DisplayName: &l.DisplayName, Username: &l.Username, Domain: &l.Domain, AuthUsername: &l.AuthUsername,
			Registrar: &l.Registrar, OutboundProxy: &l.OutboundProxy, Register: &l.Register, TLSVerify: &l.TLSVerify, Advanced: &l.Advanced,
			Enabled: &l.Enabled, Transport: given(l.Transport), Keepalive: given(l.Keepalive), MediaEncryption: given(l.MediaEncryption),
			DTMFMode: given(l.DTMFMode), CallerIDSource: given(l.CallerIDSource)}
		if len(l.Codecs) > 0 {
			in.Codecs = &l.Codecs
		}
		if l.RegisterExpiry > 0 {
			in.RegisterExpiry = &l.RegisterExpiry
		}
		if l.Password != "" {
			in.Password = &l.Password
		}
		if l.StunPassword != "" {
			in.StunPass = &l.StunPassword
		}
		note := ""
		if l.Password == "" {
			note = "no password in the file: enter it on the line's page"
		}
		var verr error
		if dry {
			verr = sipaccount.Validate(in)
		} else {
			var rec *sipaccount.Record
			if rec, verr = s.Lines.Create(ctx, u, in); verr == nil {
				byRef[l.Ref] = rec.ID
			}
		}
		var v sipaccount.ValidationError
		switch {
		case errors.As(verr, &v):
			rep.add("line", name, "fail", v.Error())
		case verr != nil:
			return nil, verr
		default:
			known[aor(l.Username, l.Domain)] = byRef[l.Ref]
			own++
			rep.add("line", name, "add", note)
		}
	}
	return byRef, nil
}

func (s *Store) importHandset(ctx context.Context, u *auth.User, h Handset, byRef map[string]uuid.UUID, dry bool, rep *Report) error {
	name := h.Name
	if name == "" {
		name = h.IPUI
	}
	ipui, err := hex.DecodeString(h.IPUI)
	if err != nil || len(ipui) == 0 || len(ipui) > 16 {
		rep.add("handset", name, "fail", "its identity is not valid")
		return nil //nolint:nilerr // a bad item is reported and the import goes on
	}
	if len(h.Name) > 64 || (h.DualRing != "" && h.DualRing != "call_waiting" && h.DualRing != "busy") {
		rep.add("handset", name, "fail", "a setting is out of range")
		return nil
	}
	lines := map[int]uuid.UUID{}
	missing := false
	for k, ref := range h.Lines {
		n, err := strconv.Atoi(k)
		if err != nil || (n != 1 && n != 2) {
			continue
		}
		if id, ok := byRef[ref]; ok {
			lines[n] = id
		} else {
			missing = true
		}
	}
	var id, owner uuid.UUID
	err = s.Pool.QueryRow(ctx, "SELECT id, owner_user_id FROM handset WHERE ipui = $1", ipui).Scan(&id, &owner)
	exists := err == nil
	if err != nil && !isNoRows(err) {
		return err
	}
	switch {
	case exists && owner != u.ID:
		rep.add("handset", name, "skip", "a handset with this identity is registered here already")
		return nil
	case !exists && h.Key == nil:
		rep.add("handset", name, "skip", "the file has no key for it: export with keys to bring a handset over")
		return nil
	}
	dl := h.DefaultLine
	if dl != nil {
		if _, ok := lines[*dl]; !ok && (!dry || !byRefHas(h, byRef, *dl)) {
			dl = nil // a default line that is not one of its lines
		}
	}
	ring := h.DualRing
	if ring == "" {
		ring = "call_waiting"
	}
	note := ""
	if missing {
		note = "a line of it is not in the file or could not be made: set it on the handset's page"
	}
	if dry {
		if exists {
			rep.add("handset", name, "update", joinNote("its settings are taken from the file; the key stays", note))
		} else if _, kerr := parseKey(h.Key); kerr != nil {
			rep.add("handset", name, "fail", kerr.Error())
		} else {
			rep.add("handset", name, "add", joinNote("registered with its key; the first base station that hears it takes it in", note))
		}
		return nil
	}
	if !exists {
		k, kerr := parseKey(h.Key)
		if kerr != nil {
			rep.add("handset", name, "fail", kerr.Error())
			return nil
		}
		in := core.Imported{IPUI: ipui, Key: k, Name: h.Name, Model: h.Model, EMC: h.EMC, DefaultLine: nil, DualRing: ring, InternalNumber: h.InternalNumber}
		if _, ok := profiles.Default.Get(h.Profile); ok {
			in.Profile = h.Profile
		}
		clearKey := func() { clear(k.UAK[:]) }
		id, err = s.Records.ImportHandset(ctx, u.ID, in)
		clearKey()
		switch {
		case errors.Is(err, core.ErrKnownIPUI):
			rep.add("handset", name, "skip", "a handset with this identity is registered here already")
			return nil
		case err != nil:
			rep.add("handset", name, "fail", "it could not be registered") // not the error text: a database error can quote a key
			return nil                                                     //nolint:nilerr // reported as an item, the import goes on
		}
	}
	cur, err := s.Handsets.Get(ctx, u, id)
	if err != nil {
		return err
	}
	up := handset.Update{Name: h.Name, DefaultLine: dl, DualRing: ring, Disclosed: cur.Disclosed, Lines: lines}
	if len(lines) == 0 {
		up.Lines = nil
		up.DefaultLine = cur.DefaultLine
	}
	if err := s.Handsets.Update(ctx, u, id, up); err != nil {
		rep.add("handset", name, "fail", "its settings could not be applied")
		return nil //nolint:nilerr // reported as an item, the import goes on
	}
	rep.ChangedHandsets = append(rep.ChangedHandsets, id)
	if exists {
		rep.add("handset", name, "update", joinNote("its settings were taken from the file; the key stayed", note))
	} else {
		rep.add("handset", name, "add", joinNote("registered with its key; the first base station that hears it takes it in", note))
	}
	return nil
}

func byRefHas(h Handset, byRef map[string]uuid.UUID, line int) bool {
	ref, ok := h.Lines[strconv.Itoa(line)]
	if !ok {
		return false
	}
	_, ok = byRef[ref]
	return ok
}

func joinNote(a, b string) string {
	if b == "" {
		return a
	}
	return a + "; " + b
}

func parseKey(k *Key) (core.Key, error) {
	var out core.Key
	if k == nil {
		return out, errors.New("no key")
	}
	uak, err := hex.DecodeString(k.UAK)
	if err != nil || len(uak) != len(out.UAK) {
		return core.Key{}, errors.New("its key is not valid")
	}
	copy(out.UAK[:], uak)
	clear(uak)
	for _, f := range []struct {
		src string
		dst *[]byte
	}{{k.CCPortID, &out.CCPortID}, {k.CCFixedID, &out.CCFixedID}} {
		b, err := hex.DecodeString(f.src)
		if err != nil || len(b) > 16 {
			return core.Key{}, errors.New("its identity data is not valid")
		}
		*f.dst = b
	}
	return out, nil
}

func (s *Store) importContacts(ctx context.Context, u *auth.User, vcf string, dry bool, rep *Report) error {
	im := contacts.ParseVCards(strings.NewReader(vcf))
	have, err := s.ownContacts(ctx, u)
	if err != nil {
		return err
	}
	var added, dup, failed int
	for _, c := range im.Contacts {
		same := false
		for _, h := range have {
			if contacts.Same(c, *h) {
				same = true
				break
			}
		}
		if same {
			dup++
			continue
		}
		if dry {
			added++
			continue
		}
		created, err := s.Contacts.Create(ctx, u, c, false)
		var v contacts.ValidationError
		switch {
		case errors.As(err, &v):
			failed++
		case err != nil:
			return err
		default:
			added++
			have = append(have, created)
		}
	}
	rep.Items = append(rep.Items, Item{Kind: "contacts", Name: "Contacts", Action: "add",
		Note: fmt.Sprintf("%d new, %d already there, %d not accepted, %d unreadable", added, dup, failed, len(im.Skipped))})
	rep.Added += added
	rep.Skipped += dup
	rep.Failed += failed
	return nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ownContacts are the user's personal contacts: not the shared ones, not those of an address book.
func (s *Store) ownContacts(ctx context.Context, u *auth.User) ([]*contacts.Contact, error) {
	all, err := s.Contacts.List(ctx, u, "", "")
	if err != nil {
		return nil, err
	}
	var mine []*contacts.Contact
	for _, c := range all {
		if c.OwnerID != nil && *c.OwnerID == u.ID && c.BookID == nil {
			mine = append(mine, c)
		}
	}
	return mine, nil
}
