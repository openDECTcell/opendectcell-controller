// SPDX-License-Identifier: AGPL-3.0-or-later

package cell

import (
	"encoding/json"
	"strings"
	"testing"
)

func f(v float64) *float64 { return &v }

func TestPublicViewHonoursEachSwitch(t *testing.T) {
	c := &Cell{Name: "Attic 7520", Profile: Profile{BoxModel: "FRITZ!Box 7520", Latitude: f(52.37593), Longitude: f(9.73201), PositionPrecision: "approx"}}
	if p := c.Public(); p.Name != "" || p.Latitude != nil || p.BoxModel != "" {
		t.Fatalf("nothing shared by default, got %+v", p)
	}
	c.ShareName = true
	if p := c.Public(); p.Name != "Attic 7520" || p.Latitude != nil || p.BoxModel != "" {
		t.Errorf("name only: %+v", p)
	}
	c.SharePosition = true
	p := c.Public()
	if p.Latitude == nil || *p.Latitude != 52.38 || *p.Longitude != 9.73 {
		t.Errorf("approx position: %+v", p)
	}
	c.PositionPrecision = "region"
	if p := c.Public(); *p.Latitude != 52.4 || *p.Longitude != 9.7 {
		t.Errorf("region position: %v %v", *p.Latitude, *p.Longitude)
	}
	c.PositionPrecision = "exact"
	if p := c.Public(); *p.Latitude != 52.37593 {
		t.Errorf("exact position: %v", *p.Latitude)
	}
	c.ShareSpecs = true
	if p := c.Public(); p.BoxModel != "FRITZ!Box 7520" {
		t.Errorf("specs: %+v", p)
	}
	b, _ := json.Marshal(c.Public())
	for _, leak := range []string{"state", "enrolled", "rfpi", "address", "ip"} {
		if strings.Contains(strings.ToLower(string(b)), leak) {
			t.Errorf("public JSON mentions %q: %s", leak, b)
		}
	}
	c.SharePosition = false
	if p := c.Public(); p.Latitude != nil || p.Longitude != nil {
		t.Error("position shared with its switch off")
	}
}
