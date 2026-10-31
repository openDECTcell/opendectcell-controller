// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
)

var png = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

func TestLogoType(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
		ok   bool
	}{
		{"png", png, true},
		{"jpeg", append([]byte("\xff\xd8\xff\xe0"), make([]byte, 32)...), true},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 32)...), true},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), false},
		{"html", []byte("<html><body>x</body></html>"), false},
		{"empty", nil, false},
		{"too big", append(append([]byte(nil), png...), make([]byte, MaxLogoBytes)...), false},
	} {
		_, err := LogoType(c.data)
		if (err == nil) != c.ok {
			t.Errorf("%s: err %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestRegistrationDomains(t *testing.T) {
	r := Registration{AllowedDomains: []string{"example.org"}}
	for email, want := range map[string]bool{"a@example.org": true, "a@EXAMPLE.org": true, "a@evil.example.org": false, "nonsense": false} {
		if got := r.AllowsEmail(email); got != want {
			t.Errorf("%s: %v, want %v", email, got, want)
		}
	}
	if !(Registration{}).AllowsEmail("x@y.z") {
		t.Error("an empty list should allow any address")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := &Store{Pool: dbtest.Migrated(t)}
	i, err := s.Instance(ctx)
	if err != nil || i.Name != DefaultName || i.HasLogo {
		t.Fatalf("default instance %+v, %v", i, err)
	}
	if err := s.Put(ctx, KeyInstance, Instance{Name: "Makerspace DECT", Tagline: "phones"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLogo(ctx, png); err != nil {
		t.Fatal(err)
	}
	i, _ = s.Instance(ctx)
	if i.Name != "Makerspace DECT" || !i.HasLogo || i.Tagline != "phones" {
		t.Fatalf("after logo %+v", i)
	}
	ct, data, _, ok, err := s.Logo(ctx)
	if err != nil || !ok || ct != "image/png" || !bytes.Equal(data, png) {
		t.Fatalf("logo %q ok=%v err=%v", ct, ok, err)
	}
	if err := s.SetLogo(ctx, []byte("<svg/>")); !errors.Is(err, ErrBadLogo) {
		t.Fatalf("svg stored: %v", err)
	}
	if err := s.ClearLogo(ctx); err != nil {
		t.Fatal(err)
	}
	i, _ = s.Instance(ctx)
	if i.HasLogo || i.Name != "Makerspace DECT" {
		t.Fatalf("after clear %+v", i)
	}
	if _, _, _, ok, _ := s.Logo(ctx); ok {
		t.Fatal("logo still there")
	}
}

func TestMapValidate(t *testing.T) {
	for _, c := range []struct {
		url, origin string
		ok          bool
	}{
		{"", "", true},
		{OSMTileURL, "https://tile.openstreetmap.org", true},
		{"https://tiles.example.org:8443/styles/{z}/{x}/{y}.png", "https://tiles.example.org:8443", true},
		{"http://tiles.example.org/{z}/{x}/{y}.png", "", false},
		{"https://tiles.example.org/{z}/{x}.png", "", false},
		{"https://user:pw@tiles.example.org/{z}/{x}/{y}.png", "", false},
		{"https://tiles.example.org/{z}/{x}/{y}.png; script-src *", "", false},
		{"https://{s}.tiles.example.org/{z}/{x}/{y}.png", "", false},
	} {
		o, err := Map{TileURL: c.url}.Validate()
		if (err == nil) != c.ok || o != c.origin {
			t.Errorf("%q: origin %q err %v", c.url, o, err)
		}
	}
}
