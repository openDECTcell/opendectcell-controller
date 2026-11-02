// SPDX-License-Identifier: AGPL-3.0-or-later

package handset

import (
	"testing"
	"time"
)

func TestPresenceAt(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	const interval = 246 * time.Second
	for _, c := range []struct {
		name                       string
		seen, detached, pageFailed *time.Time
		interval                   time.Duration
		want                       Presence
	}{
		{"never heard", nil, nil, nil, interval, Offline},
		{"heard a moment ago", ago(10 * time.Second), nil, nil, interval, Online},
		{"registered within the interval", ago(240 * time.Second), nil, nil, interval, Online},
		{"late by less than the grace", ago(interval + 20*time.Second), nil, nil, interval, Online},
		{"did not register again", ago(interval + time.Minute), nil, nil, interval, Unresponsive},
		{"unresponsive for nine minutes", ago(interval + registerGrace + 9*time.Minute), nil, nil, interval, Unresponsive},
		{"unresponsive for ten minutes", ago(interval + registerGrace + 10*time.Minute + time.Second), nil, nil, interval, Offline},
		{"no interval: silence says nothing", ago(5 * time.Hour), nil, nil, 0, Online},
		{"switched off, nothing since", ago(time.Second), ago(time.Second), nil, interval, Offline},
		{"switched off, the stamps a moment apart", ago(time.Second), ago(2 * time.Second), nil, interval, Offline},
		{"switched off, then back", ago(time.Minute), ago(time.Hour), nil, interval, Online},
		{"a page unanswered after the last contact", ago(time.Minute), nil, ago(30 * time.Second), interval, Unresponsive},
		{"a page unanswered, then heard again", ago(10 * time.Second), nil, ago(time.Minute), interval, Online},
		{"a page unanswered ten minutes ago", ago(30 * time.Minute), nil, ago(11 * time.Minute), 0, Offline},
		{"unresponsive since the earlier of the two causes", ago(interval + registerGrace + 11*time.Minute), nil, ago(time.Minute), interval, Offline},
	} {
		if got := PresenceAt(now, c.seen, c.detached, c.pageFailed, c.interval); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
