// SPDX-License-Identifier: AGPL-3.0-or-later

package profiles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogue(t *testing.T) {
	c := Default
	ms := c.Manufacturers()
	if len(ms) == 0 || ms[0].ID != "gigaset" {
		t.Fatalf("manufacturers %+v", ms)
	}
	models := c.Models("gigaset")
	got := map[string]string{}
	for _, m := range models {
		got[m.ID] = m.Name
	}
	if got["gigaset-a540"] != "Gigaset A540" || len(got) < 3 {
		t.Errorf("models %v", got)
	}
	mfr, dev, ok := c.Texts("gigaset-a540")
	if !ok || mfr == "" || dev == "" {
		t.Errorf("a device needs both texts: %v %d %d", ok, len(mfr), len(dev))
	}
	if mfr, dev, ok := c.Texts("gigaset"); !ok || mfr == "" || dev != "" {
		t.Error("a manufacturer is its own text only")
	}
	if _, _, ok := c.Texts(""); !ok {
		t.Error("no profile is the standard behaviour")
	}
	if _, _, ok := c.Texts("nokia-3310"); ok {
		t.Error("an unknown id was taken")
	}
	if c.Label("gigaset-c430a") != "Gigaset C430A" || c.Label("") != "" {
		t.Error("labels")
	}
}

// The copy here must be the library's: a profile edited in openDECTcell without
// updating this directory would otherwise change nothing in the controller.
func TestSameAsLibrary(t *testing.T) {
	dir := "../../../third_party/opendectcell/profiles"
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Skip("the opendectcell submodule is not checked out")
	}
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".profile" {
			continue
		}
		want, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		have, err := os.ReadFile(filepath.Join("data", e.Name()))
		if err != nil || string(have) != string(want) {
			t.Errorf("%s differs from third_party/opendectcell/profiles: copy it", e.Name())
		}
	}
}
