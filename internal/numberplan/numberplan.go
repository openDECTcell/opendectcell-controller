// SPDX-License-Identifier: AGPL-3.0-or-later

// Package numberplan turns the digits a person dials into the number a VoIP provider wants, and the number a call
// arrives with into the number the handset shows. It is formatting, not routing: it never chooses a line or a
// destination, and a number it does not understand goes out as it was dialled.
//
// A line carries Rules: its country code and area code, the national and international prefixes of the country, an
// outside-line digit that people dial by habit, and how to send and show numbers. The empty Rules change nothing.
package numberplan

import (
	"errors"
	"fmt"
	"strings"
)

// Formats of a dialled number on its way out (DialAs) and of an incoming number on its way to the handset (ShowAs).
const (
	AsDialled     = ""              // DialAs: only StripPrefix applies; ShowAs: as the provider sent it
	E164          = "e164"          // +493012345
	International = "international" // 00493012345 (the line's international prefix)
	National      = "national"      // 03012345 inside the country, 00... outside
)

// Rules are a line's number rules. Everything is optional.
type Rules struct {
	CountryCode string `json:"country_code,omitempty"` // "49": digits, no + and no 00
	AreaCode    string `json:"area_code,omitempty"`    // "30": without the national prefix; completes local numbers
	TrunkPrefix string `json:"trunk_prefix,omitempty"` // the national prefix: "" is "0", "-" is none (some countries)
	IntlPrefix  string `json:"intl_prefix,omitempty"`  // what is dialled for a call abroad: "" is "00"
	StripPrefix string `json:"strip_prefix,omitempty"` // an outside-line digit dialled by habit: removed
	DialAs      string `json:"dial_as,omitempty"`
	ShowAs      string `json:"show_as,omitempty"`
}

// Zero reports whether the rules change nothing.
func (r Rules) Zero() bool { return r == Rules{} }

func (r Rules) trunk() string {
	switch r.TrunkPrefix {
	case "":
		return "0"
	case "-":
		return ""
	}
	return r.TrunkPrefix
}

func (r Rules) intl() string {
	if r.IntlPrefix == "" {
		return "00"
	}
	return r.IntlPrefix
}

func digits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// Validate says what is wrong with the rules, or nil.
func (r Rules) Validate() error {
	var errs []error
	if r.CountryCode != "" && (!digits(r.CountryCode) || len(r.CountryCode) > 3 || r.CountryCode[0] == '0') {
		errs = append(errs, errors.New("the country code is one to three digits, without a 0 or a + in front (49 for Germany)"))
	}
	if r.AreaCode != "" && (!digits(r.AreaCode) || len(r.AreaCode) > 6) {
		errs = append(errs, errors.New("the area code is up to six digits, without the national prefix (30 for Berlin)"))
	}
	if r.TrunkPrefix != "" && r.TrunkPrefix != "-" && (!digits(r.TrunkPrefix) || len(r.TrunkPrefix) > 2) {
		errs = append(errs, errors.New("the national prefix is one or two digits (usually 0), or - for none"))
	}
	if r.IntlPrefix != "" && (!digits(r.IntlPrefix) || len(r.IntlPrefix) > 4) {
		errs = append(errs, errors.New("the international prefix is up to four digits (usually 00)"))
	}
	if r.StripPrefix != "" {
		switch {
		case !digits(r.StripPrefix) || len(r.StripPrefix) > 4:
			errs = append(errs, errors.New("the outside-line digits are up to four digits"))
		case strings.HasPrefix(r.intl(), r.StripPrefix) || (r.trunk() != "" && strings.HasPrefix(r.trunk(), r.StripPrefix)) ||
			strings.HasPrefix(r.StripPrefix, r.intl()) || (r.trunk() != "" && strings.HasPrefix(r.StripPrefix, r.trunk())):
			errs = append(errs, errors.New("the outside-line digits must differ from the national and international prefixes, or a number cannot be told from them"))
		}
	}
	for _, f := range []struct{ name, v string }{{"dial_as", r.DialAs}, {"show_as", r.ShowAs}} {
		switch f.v {
		case AsDialled, E164, International, National:
		default:
			errs = append(errs, fmt.Errorf("%s is empty, e164, international or national", f.name))
		}
	}
	if (r.DialAs != AsDialled || r.ShowAs != AsDialled) && r.CountryCode == "" {
		errs = append(errs, errors.New("a format needs the country code"))
	}
	if r.AreaCode != "" && r.CountryCode == "" {
		errs = append(errs, errors.New("an area code needs the country code"))
	}
	return errors.Join(errs...)
}

// minLocal is how many digits a number needs before the area code is put in front of it: shorter ones are
// extensions and service numbers.
const minLocal = 5

// passthrough: what is never touched. Codes with * or # (service codes and DTMF), numbers of up to four digits
// (emergency numbers, 112, 110, 911, 999; extensions of a PBX) and the harmonised European 116 numbers.
func passthrough(n string) bool {
	if n == "" || strings.ContainsAny(n, "*#") {
		return true
	}
	if !strings.HasPrefix(n, "+") && (len(n) < minLocal || (len(n) == 6 && strings.HasPrefix(n, "116"))) {
		return true
	}
	return false
}

// international gives the digits of n with the country code in front and no prefix of any kind (493012345), or ""
// if n is not recognised under the rules.
func (r Rules) international(n string) string {
	switch {
	case strings.HasPrefix(n, "+"):
		if digits(n[1:]) {
			return n[1:]
		}
	case strings.HasPrefix(n, r.intl()) && len(n) > len(r.intl()):
		if rest := n[len(r.intl()):]; digits(rest) {
			return rest
		}
	case r.CountryCode != "" && r.trunk() != "" && strings.HasPrefix(n, r.trunk()) && len(n) > len(r.trunk()):
		if rest := n[len(r.trunk()):]; digits(rest) {
			return r.CountryCode + rest
		}
	case r.CountryCode != "" && r.trunk() == "" && digits(n):
		// no national prefix in this country: a number that is not international is national, and complete
		return r.CountryCode + r.AreaCode + n
	case r.CountryCode != "" && r.AreaCode != "" && digits(n):
		return r.CountryCode + r.AreaCode + n // a local number
	}
	return ""
}

func (r Rules) format(intl, as string) string {
	switch as {
	case E164:
		return "+" + intl
	case International:
		return r.intl() + intl
	case National:
		if r.CountryCode != "" && strings.HasPrefix(intl, r.CountryCode) {
			return r.trunk() + intl[len(r.CountryCode):]
		}
		return r.intl() + intl
	}
	return ""
}

// Dial is the number to send for what a person dialled (typed already cleaned to digits, +, * and #).
func (r Rules) Dial(typed string) string {
	n := typed
	if r.StripPrefix != "" && strings.HasPrefix(n, r.StripPrefix) && len(n) > len(r.StripPrefix)+2 {
		n = n[len(r.StripPrefix):]
	}
	if r.DialAs == AsDialled || passthrough(n) {
		return n
	}
	if intl := r.international(n); intl != "" {
		return r.format(intl, r.DialAs)
	}
	return n
}

// Show is the number to show for what a call arrived with.
func (r Rules) Show(received string) string {
	if r.ShowAs == AsDialled || passthrough(received) {
		return received
	}
	// received numbers are complete: a number without a prefix is not made local
	if intl := (Rules{CountryCode: r.CountryCode, TrunkPrefix: r.TrunkPrefix, IntlPrefix: r.IntlPrefix}).international(received); intl != "" {
		return r.format(intl, r.ShowAs)
	}
	return received
}

// Canonical is the number in international form without a plus sign, or "" when the rules do not recognise it:
// the form in which two spellings of one number compare equal.
func (r Rules) Canonical(n string) string {
	if passthrough(n) {
		return ""
	}
	return r.international(n)
}
