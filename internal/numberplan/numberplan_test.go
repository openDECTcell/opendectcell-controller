// SPDX-License-Identifier: AGPL-3.0-or-later

package numberplan

import (
	"strings"
	"testing"
)

var berlin = Rules{CountryCode: "49", AreaCode: "30"}

func with(r Rules, f func(*Rules)) Rules { f(&r); return r }

func TestDial(t *testing.T) {
	e164 := with(berlin, func(r *Rules) { r.DialAs = E164 })
	intl := with(berlin, func(r *Rules) { r.DialAs = International })
	nat := with(berlin, func(r *Rules) { r.DialAs = National })
	for _, c := range []struct {
		name  string
		rules Rules
		typed string
		want  string
	}{
		{"national to e164", e164, "03012345678", "+493012345678"},
		{"mobile to e164", e164, "01701234567", "+491701234567"},
		{"local gets the area code", e164, "12345678", "+493012345678"},
		{"international prefix to e164", e164, "00493012345678", "+493012345678"},
		{"a plus stays", e164, "+493012345678", "+493012345678"},
		{"abroad stays abroad", e164, "0043123456", "+43123456"},
		{"national to international", intl, "03012345678", "00493012345678"},
		{"e164 to international", intl, "+493012345678", "00493012345678"},
		{"e164 to national", nat, "+493012345678", "03012345678"},
		{"abroad to national", nat, "+43123456", "0043123456"},
		{"local to national", nat, "12345678", "03012345678"},
		{"emergency", e164, "112", "112"},
		{"emergency 110", e164, "110", "110"},
		{"an extension", e164, "2001", "2001"},
		{"a service code", e164, "*21*0301234#", "*21*0301234#"},
		{"a harmonised number", e164, "116117", "116117"},
		{"empty", e164, "", ""},
		{"not recognised and left alone", with(e164, func(r *Rules) { r.AreaCode = "" }), "12345678", "12345678"},
		{"no rules", Rules{}, "03012345678", "03012345678"},
		{"as dialled does not rewrite", berlin, "03012345678", "03012345678"},
		{"another trunk prefix", with(e164, func(r *Rules) { r.TrunkPrefix = "9" }), "9301234", "+49301234"},
		{"no trunk prefix at all", with(e164, func(r *Rules) { r.TrunkPrefix = "-"; r.AreaCode = "" }), "301234567", "+49301234567"},
		{"another international prefix", with(e164, func(r *Rules) { r.IntlPrefix = "011" }), "0114930123456", "+4930123456"},
		{"outside-line digit stripped", with(e164, func(r *Rules) { r.StripPrefix = "9" }), "903012345678", "+493012345678"},
		{"strip alone, no format", Rules{StripPrefix: "9"}, "903012345678", "03012345678"},
		{"a short number keeps its digit", Rules{StripPrefix: "9"}, "911", "911"},
		{"a 9 inside a number stays", with(e164, func(r *Rules) { r.StripPrefix = "9" }), "03091234567", "+493091234567"},
	} {
		if got := c.rules.Dial(c.typed); got != c.want {
			t.Errorf("%s: Dial(%q) = %q, want %q", c.name, c.typed, got, c.want)
		}
	}
}

func TestShow(t *testing.T) {
	nat := with(berlin, func(r *Rules) { r.ShowAs = National })
	e164 := with(berlin, func(r *Rules) { r.ShowAs = E164 })
	for _, c := range []struct {
		rules    Rules
		received string
		want     string
	}{
		{nat, "+493012345678", "03012345678"},
		{nat, "00493012345678", "03012345678"},
		{nat, "03012345678", "03012345678"},
		{nat, "+43123456", "0043123456"},
		{nat, "12345678", "12345678"}, // a received number is complete: not made local
		{nat, "112", "112"},
		{nat, "anonymous", "anonymous"},
		{e164, "03012345678", "+493012345678"},
		{e164, "+43123456", "+43123456"},
		{berlin, "+493012345678", "+493012345678"},
		{Rules{}, "0049301", "0049301"},
	} {
		if got := c.rules.Show(c.received); got != c.want {
			t.Errorf("Show(%q) with %+v = %q, want %q", c.received, c.rules, got, c.want)
		}
	}
}

func TestCanonicalMakesSpellingsEqual(t *testing.T) {
	want := "493012345678"
	for _, n := range []string{"+493012345678", "00493012345678", "03012345678", "12345678"} {
		if got := berlin.Canonical(n); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", n, got, want)
		}
	}
	for _, n := range []string{"", "112", "*21#", "anonymous"} {
		if got := berlin.Canonical(n); got != "" {
			t.Errorf("Canonical(%q) = %q, want none", n, got)
		}
	}
}

func TestValidate(t *testing.T) {
	good := []Rules{{}, berlin, {CountryCode: "1", TrunkPrefix: "-", IntlPrefix: "011", DialAs: E164}, {StripPrefix: "9"}, {CountryCode: "49", StripPrefix: "9", ShowAs: National}}
	for _, r := range good {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	bad := map[string]Rules{
		"a plus in the country code":                      {CountryCode: "+49"},
		"a 0 in front":                                    {CountryCode: "049"},
		"a long country code":                             {CountryCode: "4949"},
		"a letter in the area code":                       {CountryCode: "49", AreaCode: "3a"},
		"an area code without a country":                  {AreaCode: "30"},
		"a long national prefix":                          {TrunkPrefix: "123"},
		"a long international prefix":                     {IntlPrefix: "00000"},
		"strip clashes with trunk":                        {StripPrefix: "0"},
		"strip clashes with intl":                         {StripPrefix: "00"},
		"strip starts like intl":                          {StripPrefix: "001"},
		"strip starts the international prefix only":      {CountryCode: "49", TrunkPrefix: "-", StripPrefix: "0"},
		"strip starts the national prefix only":           {CountryCode: "49", TrunkPrefix: "12", StripPrefix: "1"},
		"strip begins with the national prefix only":      {CountryCode: "49", TrunkPrefix: "9", StripPrefix: "91"},
		"strip begins with the international prefix only": {CountryCode: "49", TrunkPrefix: "-", StripPrefix: "001"},
		"strip is no digits":                              {StripPrefix: "x"},
		"an unknown format":                               {CountryCode: "49", DialAs: "pretty"},
		"a format without a country":                      {DialAs: E164},
		"show without a country":                          {ShowAs: National},
	}
	for name, r := range bad {
		if r.Validate() == nil {
			t.Errorf("%s: %+v is accepted", name, r)
		}
	}
	if err := (Rules{CountryCode: "+49", AreaCode: "3a"}).Validate(); err == nil || !strings.Contains(err.Error(), "country code") || !strings.Contains(err.Error(), "area code") {
		t.Errorf("both mistakes are not reported: %v", err)
	}
}
