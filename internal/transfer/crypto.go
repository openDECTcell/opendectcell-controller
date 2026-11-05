// SPDX-License-Identifier: AGPL-3.0-or-later

// Package transfer is the export and import of a user's setup: handsets, VoIP lines and contacts, as one file that moves to
// another controller or comes back after a loss. A file that carries secrets (handset keys, SIP passwords) is encrypted with a
// passphrase the user chooses (Argon2id and AES-256-GCM); a file without secrets is plain JSON. Nothing in this package logs a
// value from a file, and an import only ever creates things for the user who imports.
package transfer

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"

	"golang.org/x/crypto/argon2"
)

// Format names the file; Version is bumped when the layout changes in a way an older reader would misread.
const (
	Format  = "opendectcell-setup"
	Version = 1
)

// MinPassphrase is the shortest passphrase an export with secrets accepts.
const MinPassphrase = 12

// The cost of the key derivation an export writes, and the most an import accepts from a file (a file must not make the
// controller allocate gigabytes).
const (
	kdfMemoryKiB = 64 * 1024
	kdfTime      = 3
	kdfThreads   = 4

	maxMemoryKiB = 256 * 1024
	maxTime      = 10
	maxThreads   = 16
)

var (
	// ErrPassphrase is returned when an encrypted file does not open. It does not say whether the passphrase is wrong or the
	// file damaged: AES-GCM cannot tell.
	ErrPassphrase = errors.New("transfer: wrong passphrase, or the file is damaged")
	// ErrNeedPassphrase is returned for an encrypted file opened without a passphrase.
	ErrNeedPassphrase = errors.New("transfer: this file is encrypted: enter its passphrase")
	// ErrFormat is returned for a file that is not an export of this program, or one from a newer version.
	ErrFormat = errors.New("transfer: this is not a opendectcell setup file, or it is from a newer version")
	// ErrPlainSecrets is returned for a plain file that carries keys or passwords.
	ErrPlainSecrets = errors.New("transfer: this file carries keys or passwords without being encrypted; it is refused")
	// ErrWeakPassphrase is returned by an export whose passphrase is too short.
	ErrWeakPassphrase = errors.New("transfer: the passphrase is too short")
)

// envelope is the outer JSON of every file. A plain file holds Setup; an encrypted one holds Data, the sealed Setup, and the
// parameters of the key derivation. The parameters feed the key, and the whole header is also the GCM's additional data, so a file
// whose header was changed does not open.
type envelope struct {
	Format    string `json:"format"`
	Version   int    `json:"version"`
	Encrypted bool   `json:"encrypted"`

	Setup *Setup `json:"setup,omitempty"`

	KDF     string `json:"kdf,omitempty"`
	MemKiB  uint32 `json:"m,omitempty"`
	Time    uint32 `json:"t,omitempty"`
	Threads uint8  `json:"p,omitempty"`
	Salt    []byte `json:"salt,omitempty"`
	Nonce   []byte `json:"nonce,omitempty"`
	Data    []byte `json:"data,omitempty"`
}

func (e envelope) aad() []byte {
	h := e
	h.Setup, h.Data = nil, nil
	b, _ := json.Marshal(h) // a fixed struct: it cannot fail
	return b
}

func key(pass string, salt []byte, mem, t uint32, p uint8) []byte {
	return argon2.IDKey([]byte(pass), salt, t, mem, p, 32)
}

// Seal writes s as an encrypted file.
func Seal(s Setup, pass string) ([]byte, error) {
	if len([]rune(pass)) < MinPassphrase {
		return nil, ErrWeakPassphrase
	}
	plain, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	e := envelope{Format: Format, Version: Version, Encrypted: true, KDF: "argon2id", MemKiB: kdfMemoryKiB, Time: kdfTime, Threads: kdfThreads,
		Salt: make([]byte, 16), Nonce: make([]byte, 12)}
	if _, err := rand.Read(e.Salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(e.Nonce); err != nil {
		return nil, err
	}
	k := key(pass, e.Salt, e.MemKiB, e.Time, e.Threads)
	defer clear(k)
	blk, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	e.Data = gcm.Seal(nil, e.Nonce, plain, e.aad())
	return json.MarshalIndent(e, "", " ")
}

// Plain writes s as a plain file. It refuses a setup that carries secrets.
func Plain(s Setup) ([]byte, error) {
	if s.HasSecrets() {
		return nil, errors.New("transfer: a plain file cannot carry secrets")
	}
	return json.MarshalIndent(envelope{Format: Format, Version: Version, Setup: &s}, "", " ")
}

// Open reads a file, plain or encrypted. encrypted says which it was. A plain file that carries secrets is refused: no export
// writes one, so it was made by hand, and keys must not travel in the clear.
func Open(file []byte, pass string) (s Setup, encrypted bool, err error) {
	var e envelope
	if err := json.Unmarshal(file, &e); err != nil || e.Format != Format || e.Version != Version {
		return Setup{}, false, ErrFormat
	}
	if !e.Encrypted {
		if e.Setup == nil {
			return Setup{}, false, ErrFormat
		}
		if e.Setup.HasSecrets() {
			return Setup{}, false, ErrPlainSecrets
		}
		return *e.Setup, false, nil
	}
	if pass == "" {
		return Setup{}, true, ErrNeedPassphrase
	}
	if e.KDF != "argon2id" || e.MemKiB < 1024 || e.MemKiB > maxMemoryKiB || e.Time < 1 || e.Time > maxTime || e.Threads < 1 || e.Threads > maxThreads ||
		len(e.Salt) < 8 || len(e.Nonce) != 12 {
		return Setup{}, true, ErrFormat
	}
	k := key(pass, e.Salt, e.MemKiB, e.Time, e.Threads)
	defer clear(k)
	blk, err := aes.NewCipher(k)
	if err != nil {
		return Setup{}, true, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return Setup{}, true, err
	}
	plain, err := gcm.Open(nil, e.Nonce, e.Data, e.aad())
	if err != nil {
		return Setup{}, true, ErrPassphrase
	}
	defer clear(plain)
	if err := json.Unmarshal(plain, &s); err != nil {
		return Setup{}, true, ErrFormat
	}
	return s, true, nil
}
