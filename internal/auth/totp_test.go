// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1, secret "12345678901234567890", 8 digits there;
// we compare the last 6.
func TestTOTPRFCVectors(t *testing.T) {
	key := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpCode(key, uint64(unix/30)); got != want {
			t.Errorf("t=%d: %s, want %s", unix, got, want)
		}
	}
}

func TestMatchTOTPWindow(t *testing.T) {
	key, _ := hex.DecodeString("3132333435363738393031323334353637383930")
	now := time.Unix(1111111109, 0)
	cur := uint64(now.Unix() / 30)
	if matchTOTP(key, totpCode(key, cur), now) != cur {
		t.Error("current step rejected")
	}
	if matchTOTP(key, totpCode(key, cur-1), now) != cur-1 {
		t.Error("previous step rejected")
	}
	if matchTOTP(key, totpCode(key, cur+1), now) != cur+1 {
		t.Error("next step rejected")
	}
	if matchTOTP(key, totpCode(key, cur+3), now) != 0 {
		t.Error("a code three steps away accepted")
	}
	if matchTOTP(key, "12345", now) != 0 || matchTOTP(key, "abcdef", now) != 0 {
		t.Error("garbage accepted")
	}
	if matchTOTP(key, totpCode(key, cur)[:3]+" "+totpCode(key, cur)[3:], now) != cur {
		t.Error("a code typed with a space rejected")
	}
}

func TestTOTPURI(t *testing.T) {
	u := TOTPURI("Makerspace DECT", "alice", "JBSWY3DPEHPK3PXP")
	for _, want := range []string{"otpauth://totp/Makerspace%20DECT:alice?", "secret=JBSWY3DPEHPK3PXP", "issuer=Makerspace+DECT"} {
		if !strings.Contains(u, want) {
			t.Errorf("%q lacks %q", u, want)
		}
	}
}

func TestRecoveryCodeShape(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		c, err := newRecoveryCode()
		if err != nil || len(c) != 11 || c[5] != '-' || seen[c] {
			t.Fatalf("code %q %v", c, err)
		}
		seen[c] = true
		if string(recoveryHash(strings.ToUpper(c))) != string(recoveryHash(c)) {
			t.Fatal("hash depends on case")
		}
	}
}
