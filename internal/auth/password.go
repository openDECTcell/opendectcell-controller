// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ArgonParams are the Argon2id cost parameters (docs/threat-model.md §5).
type ArgonParams struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

const (
	saltLen = 16
	hashLen = 32
	// MinPasswordLen is the minimum length of a local password.
	MinPasswordLen = 12
	// MaxPasswordLen bounds the work an attacker can make us do per attempt.
	MaxPasswordLen = 256
)

var b64 = base64.RawStdEncoding

// HashPassword returns a PHC-format Argon2id hash:
// $argon2id$v=19$m=<KiB>,t=<time>,p=<threads>$<salt>$<hash>.
func HashPassword(p ArgonParams, password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, hashLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(h)), nil
}

var errBadHash = errors.New("auth: malformed password hash")

// VerifyPassword reports whether password matches the PHC hash, using the
// parameters stored in the hash.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errBadHash
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return false, errBadHash
	}
	var p ArgonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil {
		return false, errBadHash
	}
	if p.MemoryKiB == 0 || p.Time == 0 || p.Threads == 0 || p.MemoryKiB > 4<<20 || p.Time > 64 {
		return false, errBadHash
	}
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false, errBadHash
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether a hash was made with weaker parameters than p.
func NeedsRehash(encoded string, p ArgonParams) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return true
	}
	var q ArgonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &q.MemoryKiB, &q.Time, &q.Threads); err != nil {
		return true
	}
	return q.MemoryKiB < p.MemoryKiB || q.Time < p.Time
}

// CheckNewPassword applies the local password policy.
func CheckNewPassword(pw string) error {
	if len(pw) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(pw) > MaxPasswordLen {
		return fmt.Errorf("password must be at most %d characters", MaxPasswordLen)
	}
	return nil
}
