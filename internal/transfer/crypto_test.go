// SPDX-License-Identifier: AGPL-3.0-or-later

package transfer

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func secretSetup() Setup {
	return Setup{Handsets: []Handset{{Name: "Hall", IPUI: "0f", Key: &Key{UAK: strings.Repeat("ab", 16)}}},
		Lines: []Line{{Ref: "l1", Label: "Home", Username: "u", Domain: "d.example", Password: "very-secret-pw"}}}
}

func TestSealOpenRoundTrip(t *testing.T) {
	file, err := Seal(secretSetup(), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	// nothing of the secrets is readable in the file
	for _, leak := range []string{"very-secret-pw", strings.Repeat("ab", 16), "Hall", "d.example"} {
		if bytes.Contains(file, []byte(leak)) {
			t.Fatalf("the encrypted file shows %q", leak)
		}
	}
	got, enc, err := Open(file, "correct horse battery")
	if err != nil || !enc {
		t.Fatalf("open: %v (encrypted %v)", err, enc)
	}
	if got.Lines[0].Password != "very-secret-pw" || got.Handsets[0].Key.UAK != strings.Repeat("ab", 16) {
		t.Fatalf("round trip lost secrets: %+v", got)
	}
}

func TestOpenRefusals(t *testing.T) {
	file, _ := Seal(secretSetup(), "correct horse battery")
	if _, _, err := Open(file, "wrong horse battery"); !errors.Is(err, ErrPassphrase) {
		t.Errorf("wrong passphrase: %v", err)
	}
	if _, _, err := Open(file, ""); !errors.Is(err, ErrNeedPassphrase) {
		t.Errorf("no passphrase: %v", err)
	}
	// the cleartext header is authenticated: a file with the cost lowered does not open (and is not a way to a cheaper guess)
	var e envelope
	_ = json.Unmarshal(file, &e)
	e.Time = 1
	cheap, _ := json.Marshal(e)
	if _, _, err := Open(cheap, "correct horse battery"); !errors.Is(err, ErrPassphrase) {
		t.Errorf("changed header: %v", err)
	}
	// a flipped bit in the data
	e2 := envelope{}
	_ = json.Unmarshal(file, &e2)
	e2.Data[3] ^= 1
	bad, _ := json.Marshal(e2)
	if _, _, err := Open(bad, "correct horse battery"); !errors.Is(err, ErrPassphrase) {
		t.Errorf("damaged data: %v", err)
	}
	// a file may not make the controller allocate what it likes
	e3 := envelope{}
	_ = json.Unmarshal(file, &e3)
	e3.MemKiB = 4 << 20
	huge, _ := json.Marshal(e3)
	if _, _, err := Open(huge, "correct horse battery"); !errors.Is(err, ErrFormat) {
		t.Errorf("huge memory cost: %v", err)
	}
	for _, junk := range []string{"", "{}", `{"format":"other","version":1}`, `{"format":"opendectcell-setup","version":2}`, "not json"} {
		if _, _, err := Open([]byte(junk), "x"); !errors.Is(err, ErrFormat) {
			t.Errorf("%q: %v", junk, err)
		}
	}
}

func TestSecretsNeverTravelInThePlain(t *testing.T) {
	if _, err := Seal(secretSetup(), "short"); !errors.Is(err, ErrWeakPassphrase) {
		t.Errorf("short passphrase: %v", err)
	}
	if _, err := Plain(secretSetup()); err == nil {
		t.Error("a plain file was written with secrets")
	}
	// a hand-made plain file with a key is refused on reading
	raw, _ := json.Marshal(envelope{Format: Format, Version: Version, Setup: &Setup{Handsets: []Handset{{IPUI: "01", Key: &Key{UAK: "00"}}}}})
	if _, _, err := Open(raw, ""); err == nil {
		t.Error("a plain file with a key was read")
	}
	raw, _ = json.Marshal(envelope{Format: Format, Version: Version, Setup: &Setup{Lines: []Line{{Ref: "l1", Password: "x"}}}})
	if _, _, err := Open(raw, ""); err == nil {
		t.Error("a plain file with a password was read")
	}
	ok, err := Plain(Setup{Handsets: []Handset{{Name: "Hall", IPUI: "0f"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, enc, err := Open(ok, ""); err != nil || enc || got.Handsets[0].Name != "Hall" {
		t.Errorf("plain: %+v %v %v", got, enc, err)
	}
}
