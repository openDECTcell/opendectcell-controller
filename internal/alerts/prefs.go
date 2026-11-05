// SPDX-License-Identifier: AGPL-3.0-or-later

// Package alerts is the notification centre (ideas B4): conditions that are bad for long enough are told to the people they concern,
// once, and again when they are over. A user finds the notifications in the interface and, if they ask, gets them by e-mail outside
// their quiet hours.
package alerts

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// The kinds of notification. A kind is what a user switches on or off.
const (
	KindLine    = "line"    // a VoIP line cannot register
	KindHandset = "handset" // a handset has not been heard for a long time (the user chooses how long; off until they do)
	KindCell    = "cell"    // a base station is offline
	KindCert    = "cert"    // certificates end soon (administrators)
)

// KindInfo describes a kind for the page that sets the preferences.
type KindInfo struct {
	Kind, Title string
	AdminOnly   bool
	// MailByFlag: the mail for this kind is sent by the older option "base station alerts" of the account, not by this page.
	MailByFlag bool
}

// Kinds lists the kinds in the order the preferences page shows them.
var Kinds = []KindInfo{
	{Kind: KindLine, Title: "A VoIP line cannot register (for 10 minutes)"},
	{Kind: KindHandset, Title: "A handset has not been heard for a long time"},
	{Kind: KindCell, Title: "A base station is offline", MailByFlag: true},
	{Kind: KindCert, Title: "Base station certificates or the certificate authority end soon", AdminOnly: true},
}

// KindPref is a user's choice for one kind.
type KindPref struct {
	// Hide: do not list it in the interface (a missing value shows it).
	Hide bool `json:"hide,omitempty"`
	Mail bool `json:"mail,omitempty"`
}

// Prefs are a user's choices, stored in app_user.alert_prefs.
type Prefs struct {
	Kinds map[string]KindPref `json:"kinds,omitempty"`
	// HandsetHours: tell me when a handset of mine was not heard for this long; 0 is off.
	HandsetHours int `json:"handset_hours,omitempty"`
	// QuietFrom and QuietTo are "HH:MM" in the controller's local time: no mail in between (it waits for the end).
	QuietFrom string `json:"quiet_from,omitempty"`
	QuietTo   string `json:"quiet_to,omitempty"`
}

// MaxHandsetHours bounds the handset threshold: a month.
const MaxHandsetHours = 24 * 30

// ParsePrefs reads the stored JSON; damaged or empty JSON gives the defaults.
func ParsePrefs(raw []byte) Prefs {
	var p Prefs
	if len(raw) > 0 && json.Unmarshal(raw, &p) != nil {
		return Prefs{}
	}
	return p
}

// JSON is the form that is stored.
func (p Prefs) JSON() []byte {
	b, _ := json.Marshal(p) // plain values: cannot fail
	return b
}

// Shows says whether the user wants the kind in the interface.
func (p Prefs) Shows(kind string) bool { return !p.Kinds[kind].Hide }

// Mails says whether the user wants the kind by mail.
func (p Prefs) Mails(kind string) bool { return p.Kinds[kind].Mail }

// clock parses "HH:MM" into minutes since midnight.
func clock(s string) (int, bool) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// ValidClock says whether s is empty or a valid "HH:MM".
func ValidClock(s string) bool {
	if strings.TrimSpace(s) == "" {
		return true
	}
	_, ok := clock(s)
	return ok
}

// Quiet says whether t falls in the user's quiet hours. The hours may cross midnight (22:00 to 07:00). Without both ends there are none.
func (p Prefs) Quiet(t time.Time) bool {
	from, ok1 := clock(p.QuietFrom)
	to, ok2 := clock(p.QuietTo)
	if !ok1 || !ok2 || from == to {
		return false
	}
	now := t.Hour()*60 + t.Minute()
	if from < to {
		return now >= from && now < to
	}
	return now >= from || now < to
}
