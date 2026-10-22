// SPDX-License-Identifier: AGPL-3.0-or-later

package secret

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	dek, err := NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	k := NewKeyring()
	if err := k.Add(1, dek, true); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestMasterKeyRoundTrip(t *testing.T) {
	s, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	mk, err := ParseMasterKey(s + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mk.String(), s) || len(mk.ID()) != 16 {
		t.Fatalf("String/ID leak or wrong length: %q", mk.String())
	}
	for _, bad := range []string{"", "not base64!", "c2hvcnQ="} {
		if _, err := ParseMasterKey(bad); err == nil {
			t.Errorf("ParseMasterKey(%q) accepted", bad)
		}
	}
}

func TestWrapUnwrap(t *testing.T) {
	s, _ := GenerateMasterKey()
	mk, _ := ParseMasterKey(s)
	dek, _ := NewDEK()
	w, err := mk.Wrap(7, dek)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mk.Unwrap(7, w)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("Unwrap = %x, %v", got, err)
	}
	if _, err := mk.Unwrap(8, w); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrapped key moved to another id opened: %v", err)
	}
	s2, _ := GenerateMasterKey()
	mk2, _ := ParseMasterKey(s2)
	if _, err := mk2.Unwrap(7, w); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong master key opened: %v", err)
	}
}

func TestSealOpenBindsLocation(t *testing.T) {
	k := testKeyring(t)
	loc := Location{"handset", "uak_enc", "6f1c"}
	pt := []byte("0123456789abcdef")
	ct, err := k.Seal(loc, pt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, pt) {
		t.Fatal("plaintext visible in ciphertext")
	}
	got, err := k.Open(loc, ct)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("Open = %q, %v", got, err)
	}
	for _, other := range []Location{
		{"handset", "uak_enc", "6f1d"},
		{"sip_account", "uak_enc", "6f1c"},
		{"handset", "password_enc", "6f1c"},
		{"handse", "tuak_enc", "6f1c"}, // same concatenation, different split
	} {
		if _, err := k.Open(other, ct); !errors.Is(err, ErrDecrypt) {
			t.Errorf("opened at %+v: %v", other, err)
		}
	}
	ct[len(ct)-1] ^= 1
	if _, err := k.Open(loc, ct); !errors.Is(err, ErrDecrypt) {
		t.Errorf("tampered ciphertext opened: %v", err)
	}
}

func TestSealNoncesDiffer(t *testing.T) {
	k := testKeyring(t)
	loc := Location{"t", "c", "r"}
	a, _ := k.Seal(loc, []byte("x"))
	b, _ := k.Seal(loc, []byte("x"))
	if bytes.Equal(a, b) {
		t.Fatal("two seals of the same value are identical")
	}
}

func TestRotationKeepsOldKeysReadable(t *testing.T) {
	k := testKeyring(t)
	loc := Location{"t", "c", "r"}
	old, _ := k.Seal(loc, []byte("old"))
	dek2, _ := NewDEK()
	if err := k.Add(2, dek2, true); err != nil {
		t.Fatal(err)
	}
	fresh, _ := k.Seal(loc, []byte("new"))
	if id, _ := DEKID(fresh); id != 2 {
		t.Fatalf("new seal under DEK %d, want 2", id)
	}
	if id, _ := DEKID(old); id != 1 {
		t.Fatalf("old seal under DEK %d, want 1", id)
	}
	if got, err := k.Open(loc, old); err != nil || string(got) != "old" {
		t.Fatalf("old value: %q, %v", got, err)
	}
}

func TestOpenErrors(t *testing.T) {
	k := testKeyring(t)
	if _, err := k.Open(Location{}, []byte{1, 2}); !errors.Is(err, ErrDecrypt) {
		t.Errorf("short input: %v", err)
	}
	ct, _ := k.Seal(Location{}, []byte("x"))
	ct[4] = 99
	if _, err := k.Open(Location{}, ct); !errors.Is(err, ErrUnknownDEK) {
		t.Errorf("unknown DEK: %v", err)
	}
	if _, err := NewKeyring().Seal(Location{}, nil); !errors.Is(err, ErrNoActiveDEK) {
		t.Errorf("empty keyring: %v", err)
	}
}
