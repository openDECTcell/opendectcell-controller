// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"testing"
	"time"
)

func TestRoamingUnits(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want uint
	}{{0, 0}, {-time.Second, 0}, {time.Second, 1}, {41 * time.Second, 1}, {10 * time.Minute, 15}, {time.Hour, 88}, {10 * time.Hour, 255}} {
		if got := (Roaming{LocateValidity: c.d}).Units(); got != c.want {
			t.Errorf("%v: %d units, want %d", c.d, got, c.want)
		}
	}
	if d := DefaultRoaming(); d.LocateValidity != 10*time.Minute || d.ExpireAfter != time.Minute || d.MovedAfter != 3*time.Minute {
		t.Errorf("defaults %+v", d)
	}
}
