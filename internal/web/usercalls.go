// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
)

func (s *Server) userCallRoutes() {
	if s.Handsets == nil {
		return
	}
	s.mux.Handle("GET /calls", s.requireUser(s.callsPage))
	s.mux.Handle("GET /calls/export.csv", s.requireUser(s.callsExport))
	s.mux.Handle("GET /calls/{id}", s.requireUser(s.callPage))
	s.mux.Handle("POST /calls/read", s.requireUser(s.callsRead))
}

// callQuery is what the Calls page was asked for, from the query string; the filter form and the paging links
// carry it on.
type callQuery struct {
	Dir     string // "", in, out, missed
	Handset string
	Line    string
	Days    int // 0: all
	Before  int64
}

var callRanges = []int{7, 30, 90, 0}

func parseCallQuery(r *http.Request) callQuery {
	q := callQuery{Days: 30}
	switch d := r.FormValue("dir"); d {
	case "in", "out", "missed":
		q.Dir = d
	}
	q.Handset, q.Line = r.FormValue("handset"), r.FormValue("line")
	if v := r.FormValue("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			for _, ok := range callRanges {
				if n == ok {
					q.Days = n
				}
			}
		}
	}
	if b, err := strconv.ParseInt(r.FormValue("before"), 10, 64); err == nil && b > 0 {
		q.Before = b
	}
	return q
}

// Values is the query as a query string, without the paging cursor unless before is set.
func (q callQuery) Values(before int64) string {
	v := url.Values{}
	if q.Dir != "" {
		v.Set("dir", q.Dir)
	}
	if q.Handset != "" {
		v.Set("handset", q.Handset)
	}
	if q.Line != "" {
		v.Set("line", q.Line)
	}
	v.Set("days", strconv.Itoa(q.Days))
	if before > 0 {
		v.Set("before", strconv.FormatInt(before, 10))
	}
	return v.Encode()
}

// filter turns the query into a call log filter for u. A handset or line that is not the user's is ignored: the
// page shows the unfiltered list, as for any other unknown value.
func (s *Server) callFilter(r *http.Request, u *auth.User, q *callQuery, handsets map[uuid.UUID]string, lines map[uuid.UUID]string) calllog.Filter {
	f := calllog.Filter{Owner: &u.ID, Before: q.Before, Limit: 50}
	switch q.Dir {
	case "in", "out":
		f.Direction = q.Dir
	case "missed":
		f.Missed = true
	}
	if id, err := uuid.Parse(q.Handset); err == nil {
		if _, mine := handsets[id]; mine {
			f.Handset = &id
		} else {
			q.Handset = ""
		}
	} else {
		q.Handset = ""
	}
	if id, err := uuid.Parse(q.Line); err == nil {
		if _, mine := lines[id]; mine {
			f.Account = &id
		} else {
			q.Line = ""
		}
	} else {
		q.Line = ""
	}
	if q.Days > 0 {
		f.Since = time.Now().AddDate(0, 0, -q.Days)
	}
	return f
}

// callRow is a call as the page shows it.
type callRow struct {
	calllog.Call
	HandsetName string
	LineName    string
	Result      string // missed, answered, not answered, ongoing
	ResultClass string // a badge colour
	MOSClass    string
}

// callResult says in a word how a call went, and the colour of its badge.
func callResult(c calllog.Call) (string, string) {
	switch {
	case c.AnsweredAt != nil && c.EndedAt != nil:
		return "answered", "bg-success-lt"
	case c.AnsweredAt != nil:
		return "ongoing", "bg-blue-lt"
	case c.EndedAt == nil:
		if time.Since(c.StartedAt) < 6*time.Hour {
			return "ringing", "bg-blue-lt"
		}
		return "unknown", "bg-secondary-lt"
	case c.Direction == "in":
		return "missed", "bg-danger-lt"
	}
	return "not answered", "bg-warning-lt"
}

// mosClass colours a MOS value: 4 and above is good, 3.5 and above acceptable (the scale is the narrowband one of
// ITU-T G.107, 1.0 to 4.5).
func mosClass(mos float64) string {
	switch {
	case mos >= 4:
		return "bg-success-lt"
	case mos >= 3.5:
		return "bg-warning-lt"
	}
	return "bg-danger-lt"
}

// endReasonText is an end reason in words; one this version does not know is shown as it is.
func endReasonText(reason string) string {
	switch reason {
	case "normal":
		return "ended normally"
	case "hs":
		return "the handset hung up"
	case "cancelled":
		return "the caller hung up before it was answered"
	case "no_answer", "missed":
		return "nobody answered"
	case "busy":
		return "busy"
	case "unreachable":
		return "the other side could not be reached"
	case "declined", "rejected":
		return "declined"
	case "cell":
		return "the base station went away"
	}
	return reason
}

type callsView struct {
	Rows       []callRow
	Query      callQuery
	Handsets   map[uuid.UUID]string
	HandsetIDs []uuid.UUID // in the order of the menu
	Lines      []lineChoice
	NextBefore int64
	More       string // the query string for the older page
	Export     string // the query string for the export
	Unread     int
	Ranges     []int
}

type lineChoice struct {
	ID    uuid.UUID
	Label string
}

// names are the user's handsets and VoIP lines by id: the words for a call's handset and line.
func (s *Server) callNames(r *http.Request, u *auth.User) (handsets map[uuid.UUID]string, order []uuid.UUID, lines map[uuid.UUID]string, choices []lineChoice, err error) {
	hs, err := s.Handsets.List(r.Context(), u)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	handsets = map[uuid.UUID]string{}
	for _, h := range hs {
		if h.OwnerID == u.ID {
			handsets[h.ID] = h.Display()
			order = append(order, h.ID)
		}
	}
	accts, err := s.Handsets.Accounts(r.Context(), u.ID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	lines = map[uuid.UUID]string{}
	for _, a := range accts {
		lines[a.ID] = a.Label
		choices = append(choices, lineChoice{a.ID, a.Label})
	}
	return handsets, order, lines, choices, nil
}

func (s *Server) rowsFor(calls []calllog.Call, handsets, lines map[uuid.UUID]string) []callRow {
	rows := make([]callRow, len(calls))
	for i, c := range calls {
		row := callRow{Call: c}
		if c.HandsetID != nil {
			row.HandsetName = handsets[*c.HandsetID]
		}
		if c.SIPAccountID != nil {
			row.LineName = lines[*c.SIPAccountID]
		}
		row.Result, row.ResultClass = callResult(c)
		if c.Quality != nil {
			row.MOSClass = mosClass(c.Quality.MOS)
		}
		rows[i] = row
	}
	return rows
}

func (s *Server) callsPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	handsets, order, lines, choices, err := s.callNames(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	q := parseCallQuery(r)
	f := s.callFilter(r, u, &q, handsets, lines)
	store := &calllog.Store{Pool: s.Pool}
	calls, err := store.List(r.Context(), f)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	unread, err := store.UnreadMissed(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	d := callsView{Rows: s.rowsFor(calls, handsets, lines), Query: q, Handsets: handsets, HandsetIDs: order, Lines: choices,
		Unread: unread, Ranges: callRanges, Export: q.Values(0)}
	if len(calls) == f.Limit {
		d.NextBefore = calls[len(calls)-1].ID
		d.More = q.Values(d.NextBefore)
	}
	s.render(w, http.StatusOK, "calls.html", page{Title: "Calls", User: u, Data: d, Notice: r.FormValue("done")})
}

// callDetail is one call with everything the log holds, for the page that explains a bad one.
type callDetail struct {
	callRow
	Cells []string
}

func (s *Server) callPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, u)
		return
	}
	c, err := (&calllog.Store{Pool: s.Pool}).Get(r.Context(), id)
	if errors.Is(err, calllog.ErrNotFound) || (err == nil && (c.OwnerID == nil || *c.OwnerID != u.ID)) {
		s.notFound(w, r, u)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	handsets, _, lines, _, err := s.callNames(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	d := callDetail{callRow: s.rowsFor([]calllog.Call{c}, handsets, lines)[0]}
	if s.Cells != nil && len(c.CellIDs) > 0 {
		names := map[uuid.UUID]string{}
		if cells, err := s.Cells.ListCells(r.Context(), u); err == nil {
			for _, cl := range cells {
				names[cl.ID] = cl.Name
			}
		}
		for _, cid := range c.CellIDs {
			if n := names[cid]; n != "" {
				d.Cells = append(d.Cells, n)
			} else {
				d.Cells = append(d.Cells, "another base station")
			}
		}
	}
	s.render(w, http.StatusOK, "call.html", page{Title: "Call", User: u, Data: d})
}

func (s *Server) callsRead(w http.ResponseWriter, r *http.Request, u *auth.User) {
	n, err := (&calllog.Store{Pool: s.Pool}).MarkMissedRead(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/calls?done="+url.QueryEscape(fmt.Sprintf("%d missed calls marked as read.", n)), http.StatusSeeOther)
}

// csvSafe keeps a spreadsheet from running what a cell holds: text that starts like a formula gets an apostrophe in
// front (OWASP, CSV injection). A telephone number, a plus sign and digits, stays as it is.
func csvSafe(v string) string {
	if v == "" {
		return v
	}
	switch v[0] {
	case '=', '-', '@', '\t', '\r':
		return "'" + v
	case '+':
		if strings.Trim(v[1:], "0123456789 ") != "" {
			return "'" + v
		}
	}
	return v
}

const csvMaxRows = 10000

func (s *Server) callsExport(w http.ResponseWriter, r *http.Request, u *auth.User) {
	handsets, _, lines, _, err := s.callNames(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	q := parseCallQuery(r)
	q.Before = 0
	f := s.callFilter(r, u, &q, handsets, lines)
	f.Limit = 500
	store := &calllog.Store{Pool: s.Pool}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="calls-`+time.Now().Format("2006-01-02")+`.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte("\xEF\xBB\xBF")) // so a spreadsheet reads umlauts in handset names as UTF-8
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"started", "direction", "number", "handset", "line", "answered", "duration_seconds", "result", "ended_because", "sip_codec", "cell_codec", "encrypted_srtp", "mos"})
	n := 0
	for n < csvMaxRows {
		calls, err := store.List(r.Context(), f)
		if err != nil {
			// the headers are out: the file ends here, and the log has the reason
			s.Log.Error("calls export", "err", err)
			break
		}
		for _, row := range s.rowsFor(calls, handsets, lines) {
			mos := ""
			if row.Quality != nil {
				mos = strconv.FormatFloat(row.Quality.MOS, 'f', 1, 64)
			}
			_ = cw.Write([]string{row.StartedAt.Format(time.RFC3339), row.Direction, csvSafe(row.RemoteParty), csvSafe(row.HandsetName), csvSafe(row.LineName),
				strconv.FormatBool(row.AnsweredAt != nil), strconv.FormatInt(int64(row.Duration().Seconds()), 10), row.Result, deref0(row.EndReason),
				deref0(row.SIPCodec), deref0(row.CellCodec), strconv.FormatBool(row.SRTP != nil && *row.SRTP), mos})
			n++
		}
		if len(calls) < f.Limit {
			break
		}
		f.Before = calls[len(calls)-1].ID
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		s.Log.Info("calls export: the client went away", "err", err)
	}
}

func deref0(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
