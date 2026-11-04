// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

// overview is what the start page shows: the state of a person's lines, handsets and base stations at a glance, the
// last calls and the calls going on, and what needs attention.
type overview struct {
	Summary  stats.Summary
	HasStats bool

	Lines   []overviewLine
	LinesUp int

	Handsets       []overviewHandset
	HandsetsOnline int

	Stations       []overviewStation
	StationsOnline int

	Recent []callRow
	Active []activeCall
	Unread int

	Attention []attention
	Steps     []homeStep
	StepsDone bool

	System *overviewSystem // administrators only
}

type overviewLine struct {
	ID     uuid.UUID
	Label  string
	State  string // registered, registering, failed, backoff, disabled, not running
	Class  string // the badge colour
	Error  string
	Shared bool
	Uses   int // handsets that have it as a line
}

type overviewHandset struct {
	ID       uuid.UUID
	Name     string
	Presence handset.Presence
	Signal   string // a word and the value, "good (-70 dBm)"
	LastSeen *time.Time
	Cell     string
	Lines    int
}

type overviewStation struct {
	ID       uuid.UUID
	Name     string
	Model    string
	Online   bool
	LastSeen *time.Time
	Active   int // calls on it now
	Capacity int // what it announced, 0 unknown
}

type activeCall struct {
	Direction string
	Number    string
	Handset   string
	Line      string
	Since     time.Time
	Answered  bool
}

// attention is one thing that needs a person's attention. Level is info, warning or danger.
type attention struct {
	Level string
	Text  string
	Href  string
	Label string
}

type overviewSystem struct {
	Version       string
	Up            string
	DBSize        string
	Users         int
	UsersPending  int
	CertsExpiring int
	MailOn        bool
	ActiveCalls   int
	Calls24h      int
}

// regBadge is the word and the colour for a line's registration state.
func regBadge(st sip.RegStatus, known bool) (string, string) {
	if !known {
		return "not running", "bg-secondary-lt"
	}
	switch st.State {
	case sip.RegRegistered:
		return "registered", "bg-success-lt"
	case sip.RegRegistering:
		return "registering", "bg-blue-lt"
	case sip.RegFailed, sip.RegBackoff:
		return "failing", "bg-danger-lt"
	case sip.RegDisabled:
		return "off", "bg-secondary-lt"
	}
	return string(st.State), "bg-secondary-lt"
}

// handsetQuiet is how long a handset may be unheard before the overview asks about it.
const handsetQuiet = 24 * time.Hour

func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "a moment"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// buildOverview gathers the start page for u. A part whose source fails is left out and logged: the page is better
// partial than an error.
func (s *Server) buildOverview(ctx context.Context, u *auth.User) overview {
	now := time.Now()
	var o overview
	if s.Stats != nil {
		if m, err := s.Stats.Summary(ctx, stats.Own(u.ID)); err == nil {
			o.Summary, o.HasStats = m, true
		}
	}
	// VoIP lines: the user's own and the shared ones their handsets use
	if s.SIPAccounts != nil {
		if recs, err := s.SIPAccounts.List(ctx, u, u.ID.String()); err == nil {
			ids := make([]uuid.UUID, len(recs))
			for i, r := range recs {
				ids[i] = r.ID
			}
			uses, _ := s.SIPAccounts.Uses(ctx, u, ids)
			for _, r := range recs {
				st, known := s.sipStatus(r.ID)
				word, class := regBadge(st, known)
				l := overviewLine{ID: r.ID, Label: r.Label, State: word, Class: class, Error: st.LastError, Shared: r.OwnerID == nil, Uses: len(uses[r.ID])}
				if !r.Enabled {
					l.State, l.Class = "off", "bg-secondary-lt"
				}
				if word == "registered" && r.Enabled {
					o.LinesUp++
				}
				o.Lines = append(o.Lines, l)
				if r.Enabled && word == "failing" {
					msg := fmt.Sprintf("The VoIP line %s does not register", r.Label)
					if st.LastError != "" {
						msg += ": " + st.LastError
					}
					o.Attention = append(o.Attention, attention{"danger", msg, "/sip-accounts/" + r.ID.String(), "Open the line"})
				}
			}
		}
	}
	// Handsets
	if s.Handsets != nil {
		if hs, err := s.Handsets.List(ctx, u); err == nil {
			for _, h := range hs {
				if h.OwnerID != u.ID {
					continue
				}
				oh := overviewHandset{ID: h.ID, Name: h.Display(), Presence: h.Presence, LastSeen: h.LastSeenAt, Cell: h.LastSeenCell, Lines: len(h.Lines)}
				if h.SignalDBm != nil {
					oh.Signal = fmt.Sprintf("%s (%d dBm)", handset.SignalWord(*h.SignalDBm), *h.SignalDBm)
				}
				if h.Presence == handset.Online {
					o.HandsetsOnline++
				}
				o.Handsets = append(o.Handsets, oh)
				href := "/handsets/" + h.ID.String()
				switch {
				case h.CellsHoldingN == 0 && !h.Pooled:
					o.Attention = append(o.Attention, attention{"warning", fmt.Sprintf("The handset %s is not registered on any base station", oh.Name), href, "Open the handset"})
				case h.LastSeenAt == nil:
					o.Attention = append(o.Attention, attention{"info", fmt.Sprintf("The handset %s has never been heard", oh.Name), href, "Open the handset"})
				case now.Sub(*h.LastSeenAt) > handsetQuiet && h.Presence != handset.Online:
					o.Attention = append(o.Attention, attention{"warning", fmt.Sprintf("The handset %s has not been heard for %s", oh.Name, ago(now, *h.LastSeenAt)), href, "Open the handset"})
				}
				if len(h.Lines) == 0 {
					o.Attention = append(o.Attention, attention{"info", fmt.Sprintf("The handset %s has no VoIP line, so it cannot call out", oh.Name), href, "Give it a line"})
				}
			}
		}
	}
	// Base stations
	cl := &calllog.Store{Pool: s.Pool}
	active, _ := cl.Active(ctx, &u.ID)
	onStation := map[uuid.UUID]int{}
	for _, c := range active {
		for _, id := range c.CellIDs {
			onStation[id]++
		}
	}
	if s.Cells != nil {
		if cells, err := s.Cells.ListOwned(ctx, u); err == nil {
			for _, c := range cells {
				live := s.liveStatus(c.ID)
				os := overviewStation{ID: c.ID, Name: c.Name, Model: c.Model(), Online: live.Online, LastSeen: c.LastSeenAt, Active: onStation[c.ID]}
				if c.CapAudioNB != nil {
					os.Capacity = int(*c.CapAudioNB)
				}
				if live.Online {
					o.StationsOnline++
				}
				o.Stations = append(o.Stations, os)
				if c.State == "active" && !live.Online {
					msg := fmt.Sprintf("The base station %s is offline", c.Name)
					if c.LastSeenAt != nil {
						msg += " (last seen " + ago(now, *c.LastSeenAt) + " ago)"
					} else {
						msg = fmt.Sprintf("The base station %s has never connected", c.Name)
					}
					o.Attention = append(o.Attention, attention{"danger", msg, "/stations/" + c.ID.String(), "Open the base station"})
				}
			}
		}
	}
	// Calls: the last five, the ones going on, the missed ones nobody has seen
	handsetNames := map[uuid.UUID]string{}
	for _, h := range o.Handsets {
		handsetNames[h.ID] = h.Name
	}
	lineNames := map[uuid.UUID]string{}
	for _, l := range o.Lines {
		lineNames[l.ID] = l.Label
	}
	if calls, err := cl.List(ctx, calllog.Filter{Owner: &u.ID, Limit: 5}); err == nil {
		o.Recent = s.rowsFor(calls, handsetNames, lineNames, s.contactNames(ctx, u.ID, calls))
	}
	for _, c := range active {
		a := activeCall{Direction: c.Direction, Number: c.RemoteParty, Since: c.StartedAt, Answered: c.AnsweredAt != nil}
		if c.HandsetID != nil {
			a.Handset = handsetNames[*c.HandsetID]
		}
		if c.SIPAccountID != nil {
			a.Line = lineNames[*c.SIPAccountID]
		}
		o.Active = append(o.Active, a)
	}
	if n, err := cl.UnreadMissed(ctx, u.ID); err == nil && n > 0 {
		o.Unread = n
		o.Attention = append(o.Attention, attention{"warning", fmt.Sprintf("%d missed %s not seen yet", n, map[bool]string{true: "call", false: "calls"}[n == 1]), "/calls?dir=missed", "Show them"})
	}
	// The first-run list, until it is done
	o.Steps = []homeStep{
		{Done: len(o.Stations) > 0, Text: "A base station is connected", Href: "/stations", Label: "Base stations"},
		{Done: len(o.Lines) > 0, Text: "A VoIP line (SIP account) is set up", Href: "/sip-accounts", Label: "VoIP lines"},
		{Done: len(o.Handsets) > 0, Text: "A handset is registered", Href: "/handsets", Label: "Handsets"},
	}
	o.StepsDone = true
	for _, st := range o.Steps {
		o.StepsDone = o.StepsDone && st.Done
	}
	if u.IsAdmin() {
		o.System = s.overviewSystem(ctx, &o)
	}
	sort.SliceStable(o.Attention, func(i, j int) bool { return attentionRank[o.Attention[i].Level] < attentionRank[o.Attention[j].Level] })
	return o
}

var attentionRank = map[string]int{"danger": 0, "warning": 1, "info": 2}

// overviewSystem is the administrators' card about the controller itself, and the attention items that are theirs.
func (s *Server) overviewSystem(ctx context.Context, o *overview) *overviewSystem {
	sys := &overviewSystem{Version: s.Version, MailOn: s.mailOn()}
	if !s.StartedAt.IsZero() {
		sys.Up = time.Since(s.StartedAt).Truncate(time.Minute).String()
	}
	var size int64
	if err := s.Pool.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&size); err == nil {
		sys.DBSize = fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	}
	if s.Stats != nil {
		if f, err := s.Stats.Fleet(ctx); err == nil {
			sys.Users, sys.UsersPending, sys.CertsExpiring = f.Users, f.UsersPending, f.CertsExpiring
		}
		if m, err := s.Stats.Summary(ctx, stats.Scope{}); err == nil {
			sys.Calls24h = m.Calls24h
		}
	}
	if all, err := (&calllog.Store{Pool: s.Pool}).Active(ctx, nil); err == nil {
		sys.ActiveCalls = len(all)
	}
	if sys.CertsExpiring > 0 {
		o.Attention = append(o.Attention, attention{"danger", fmt.Sprintf("%d base station certificates end within 14 days and could not renew", sys.CertsExpiring), "/cells", "Base stations"})
	}
	if sys.UsersPending > 0 {
		o.Attention = append(o.Attention, attention{"info", fmt.Sprintf("%d sign-ups wait for approval", sys.UsersPending), "/admin/users", "Users"})
	}
	if !sys.MailOn {
		o.Attention = append(o.Attention, attention{"info", "No mail server is set up: password resets, alerts and missed-call mails cannot be sent", "/admin/settings", "Settings"})
	}
	return sys
}
