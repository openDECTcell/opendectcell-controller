// SPDX-License-Identifier: AGPL-3.0-or-later

package handset

import (
	"time"
)

// Presence is whether a handset can be reached, as the lists show it.
type Presence string

// The three states. A handset is Online while it is heard within its registration interval; it is
// Unresponsive when it is not (it did not register again in time, or nobody answered a page for
// it) and Offline when it said it was switched off or has been unresponsive for OfflineAfter.
const (
	Online       Presence = "online"
	Unresponsive Presence = "unresponsive"
	Offline      Presence = "offline"
)

// Label is the word shown.
func (p Presence) Label() string {
	switch p {
	case Online:
		return "Online"
	case Unresponsive:
		return "Unresponsive"
	}
	return "Offline"
}

// OfflineAfter is how long a handset stays unresponsive before it is shown as offline.
const OfflineAfter = 10 * time.Minute

// registerGrace is the time a late re-registration is allowed on top of the interval (a radio
// retry, a handset busy in a call): the handset is not called unresponsive for a moment's delay.
const registerGrace = 30 * time.Second

// detachSlack: a {DETACH} and the contact it counts as are stamped from one message but not at one
// instant; the handset is detached when it sent DETACH within this of its last contact.
const detachSlack = 2 * time.Second

// PresenceAt works out a handset's presence at now from what the DECT core noted of it: when it
// was last heard, when it last said it was switched off, when nobody answered a page for it last,
// and the interval it is asked to register again in (0: it is not, so silence says nothing).
func PresenceAt(now time.Time, lastSeen, detached, pageFailed *time.Time, interval time.Duration) Presence {
	if lastSeen == nil {
		return Offline // never heard
	}
	if detached != nil && !detached.Add(detachSlack).Before(*lastSeen) {
		return Offline // it said so, and nothing since
	}
	var since *time.Time // when it stopped responding
	if pageFailed != nil && pageFailed.After(*lastSeen) {
		since = pageFailed
	}
	if interval > 0 {
		if due := lastSeen.Add(interval + registerGrace); now.After(due) && (since == nil || due.Before(*since)) {
			since = &due
		}
	}
	switch {
	case since == nil:
		return Online
	case now.Sub(*since) >= OfflineAfter:
		return Offline
	}
	return Unresponsive
}
