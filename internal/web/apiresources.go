// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

// The handsets, calls and statistics of the API (what personal API tokens are mostly for). They use the stores the pages use, for
// the user the request is made by: nobody sees another user's handsets or calls here.
func (s *Server) resourceRoutes() {
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request, *auth.User)) {
		s.mux.Handle(pattern, s.apiUser(fn))
	}
	if s.Handsets != nil {
		h("GET "+apiPrefix+"/handsets", s.apiHandsets)
		h("GET "+apiPrefix+"/handsets/{id}", s.apiHandsetGet)
		h("PATCH "+apiPrefix+"/handsets/{id}", s.apiHandsetPatch)
	}
	h("GET "+apiPrefix+"/calls", s.apiCalls)
	h("GET "+apiPrefix+"/calls/{id}", s.apiCallGet)
	if s.Stats != nil {
		h("GET "+apiPrefix+"/statistics/summary", s.apiStatsSummary)
	}
}

type apiHandsetLine struct {
	Line      int       `json:"line"`
	AccountID uuid.UUID `json:"sip_account_id"`
	Label     string    `json:"display_name"`
	Address   string    `json:"address"`
	Enabled   bool      `json:"enabled"`
}

type apiHandset struct {
	ID             uuid.UUID        `json:"id"`
	Name           string           `json:"display_name"`
	Model          string           `json:"model"`
	Profile        string           `json:"profile"`
	RegisteredAt   time.Time        `json:"registered_at"`
	LastSeenAt     *time.Time       `json:"last_seen_at"`
	LastSeenCell   string           `json:"last_seen_cell"`
	Presence       string           `json:"presence"`
	SignalDBm      *int             `json:"signal_dbm"`
	DefaultLine    *int             `json:"default_line"`
	DualRing       string           `json:"dual_ring_policy"`
	InternalNumber int              `json:"internal_number"`
	Lines          []apiHandsetLine `json:"lines"`
}

func handsetJSON(h *handset.Handset) apiHandset {
	out := apiHandset{ID: h.ID, Name: h.Name, Model: h.Model, Profile: h.Profile, RegisteredAt: h.RegisteredAt, LastSeenAt: h.LastSeenAt, LastSeenCell: h.LastSeenCell,
		Presence: string(h.Presence), SignalDBm: h.SignalDBm, DefaultLine: h.DefaultLine, DualRing: h.DualRing, InternalNumber: h.InternalNumber, Lines: []apiHandsetLine{}}
	for _, l := range h.Lines {
		out.Lines = append(out.Lines, apiHandsetLine{Line: l.Line, AccountID: l.AccountID, Label: l.Label, Address: l.Address, Enabled: l.Enabled})
	}
	return out
}

func (s *Server) apiHandsets(w http.ResponseWriter, r *http.Request, u *auth.User) {
	hs, err := s.Handsets.List(r.Context(), u)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	items := make([]apiHandset, 0, len(hs))
	for _, h := range hs {
		items = append(items, handsetJSON(h))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next": nil})
}

func (s *Server) apiHandsetGet(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, err := s.Handsets.Get(r.Context(), u, id)
	if errors.Is(err, handset.ErrNotFound) {
		notFoundAPI(w)
		return
	}
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, handsetJSON(h))
}

// apiHandsetPatch changes what the handset page's form changes (name, ring policy, default line, which VoIP line is on which key). A
// field that is left out stays as it is; "lines" gives the account of line 1 and line 2 (null clears a line).
func (s *Server) apiHandsetPatch(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Name        *string               `json:"display_name"`
		DualRing    *string               `json:"dual_ring_policy"`
		DefaultLine optInt                `json:"default_line"`
		Lines       map[string]*uuid.UUID `json:"lines"`
	}
	if !decode(w, r, &in) {
		return
	}
	cur, err := s.Handsets.Get(r.Context(), u, id)
	if errors.Is(err, handset.ErrNotFound) {
		notFoundAPI(w)
		return
	}
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	up := handset.Update{Name: cur.Name, DefaultLine: cur.DefaultLine, DualRing: cur.DualRing, Disclosed: cur.Disclosed}
	if in.Name != nil {
		up.Name = strings.TrimSpace(*in.Name)
	}
	if in.DualRing != nil {
		up.DualRing = *in.DualRing
	}
	if in.DefaultLine.Set {
		up.DefaultLine = in.DefaultLine.V
	}
	if in.Lines != nil {
		up.Lines = map[int]uuid.UUID{}
		for k, v := range in.Lines {
			n, err := strconv.Atoi(k)
			if err != nil || (n != 1 && n != 2) {
				problem(w, http.StatusBadRequest, "invalid", "", map[string]string{"lines": "the keys are \"1\" and \"2\""})
				return
			}
			up.Lines[n] = uuid.Nil
			if v != nil {
				up.Lines[n] = *v
			}
		}
	}
	if err := s.Handsets.Update(r.Context(), u, id, up); err != nil {
		if errors.Is(err, handset.ErrInvalid) {
			problem(w, http.StatusBadRequest, "invalid", err.Error(), nil)
			return
		}
		s.apiError(w, r, err)
		return
	}
	if s.HandsetChanged != nil && up.Name != cur.Name {
		s.HandsetChanged(id)
	}
	h, err := s.Handsets.Get(r.Context(), u, id)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, handsetJSON(h))
}

// optInt is a number that may be left out (unchanged), null (none) or given.
type optInt struct {
	Set bool
	V   *int
}

// UnmarshalJSON notes that the field was there, and whether it was null.
func (o *optInt) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.V = nil
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	o.V = &n
	return nil
}

type apiCall struct {
	ID              int64      `json:"id"`
	Direction       string     `json:"direction"`
	Remote          string     `json:"remote_party"`
	StartedAt       time.Time  `json:"started_at"`
	AnsweredAt      *time.Time `json:"answered_at"`
	EndedAt         *time.Time `json:"ended_at"`
	DurationSeconds int64      `json:"duration_seconds"`
	Result          string     `json:"result"`
	EndReason       *string    `json:"end_reason"`
	HandsetID       *uuid.UUID `json:"handset_id"`
	Line            *int       `json:"line"`
	LineID          *uuid.UUID `json:"sip_account_id"`
	SIPCodec        *string    `json:"sip_codec"`
	CellCodec       *string    `json:"cell_codec"`
	SRTP            *bool      `json:"srtp"`
	MOS             *float64   `json:"mos"`
	MissedRead      *bool      `json:"missed_read"`
}

func callJSON(c calllog.Call) apiCall {
	result, _ := callResult(c)
	out := apiCall{ID: c.ID, Direction: c.Direction, Remote: c.RemoteParty, StartedAt: c.StartedAt, AnsweredAt: c.AnsweredAt, EndedAt: c.EndedAt,
		DurationSeconds: int64(c.Duration() / time.Second), Result: result, EndReason: c.EndReason, HandsetID: c.HandsetID, Line: c.Line, LineID: c.SIPAccountID,
		SIPCodec: c.SIPCodec, CellCodec: c.CellCodec, SRTP: c.SRTP}
	if c.Quality != nil {
		mos := c.Quality.MOS
		out.MOS = &mos
	}
	if c.Missed() {
		read := c.MissedReadAt != nil
		out.MissedRead = &read
	}
	return out
}

// apiCalls lists the user's own calls, newest first. Filters: since (RFC 3339), direction (in or out), missed (true), line and handset
// (ids), limit (1 to 500, default 50), before (the id of the last call of the previous page).
func (s *Server) apiCalls(w http.ResponseWriter, r *http.Request, u *auth.User) {
	f := calllog.Filter{Owner: &u.ID, Limit: 50}
	q := r.URL.Query()
	fields := map[string]string{}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			fields["since"] = "a time like 2026-11-01T00:00:00Z"
		}
		f.Since = t
	}
	switch v := q.Get("direction"); v {
	case "", "in", "out":
		f.Direction = v
	default:
		fields["direction"] = "in or out"
	}
	switch q.Get("missed") {
	case "", "false":
	case "true":
		f.Missed = true
	default:
		fields["missed"] = "true or false"
	}
	for name, dst := range map[string]**uuid.UUID{"line": &f.Account, "handset": &f.Handset} {
		if v := q.Get(name); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				fields[name] = "an id"
				continue
			}
			*dst = &id
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			fields["limit"] = "1 to 500"
		}
		f.Limit = n
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			fields["before"] = "a call id"
		}
		f.Before = n
	}
	if len(fields) > 0 {
		problem(w, http.StatusBadRequest, "invalid", "", fields)
		return
	}
	calls, err := (&calllog.Store{Pool: s.Pool}).List(r.Context(), f)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	items := make([]apiCall, 0, len(calls))
	for _, c := range calls {
		items = append(items, callJSON(c))
	}
	var next any
	if len(calls) == f.Limit {
		next = calls[len(calls)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next": next})
}

func (s *Server) apiCallGet(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFoundAPI(w)
		return
	}
	c, err := (&calllog.Store{Pool: s.Pool}).Get(r.Context(), id)
	if err != nil || c.OwnerID == nil || *c.OwnerID != u.ID {
		notFoundAPI(w)
		return
	}
	writeJSON(w, http.StatusOK, callJSON(c))
}

// apiStatsSummary is the user's numbers: the headline figures and the calls per day of the last days (default 30; 1 to 90).
func (s *Server) apiStatsSummary(w http.ResponseWriter, r *http.Request, u *auth.User) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 90 {
			problem(w, http.StatusBadRequest, "invalid", "", map[string]string{"days": "1 to 90"})
			return
		}
		days = n
	}
	sc := stats.Own(u.ID)
	sum, err := s.Stats.Summary(r.Context(), sc)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	series, err := s.Stats.Days(r.Context(), sc, days)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	type day struct {
		Date        string `json:"date"`
		Calls       int    `json:"calls"`
		Answered    int    `json:"answered"`
		TalkSeconds int64  `json:"talk_seconds"`
	}
	ds := make([]day, 0, len(series))
	for _, d := range series {
		ds = append(ds, day{d.Day.Format("2006-01-02"), d.Calls, d.Answered, d.TalkSeconds})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"handsets": sum.Handsets, "lines": sum.SIPAccounts, "calls_total": sum.CallsTotal,
		"calls_24h": sum.Calls24h, "calls_7d": sum.Calls7d, "answered_7d": sum.Answered7d, "failed_7d": sum.Failed7d,
		"talk_seconds_7d": sum.TalkSeconds7d, "encrypted_7d": sum.Encrypted7d, "days": ds,
	})
}
