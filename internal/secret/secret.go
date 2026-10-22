// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secret implements envelope encryption for secrets stored in the
// database (docs/threat-model.md §4).
//
// A master key (KEK) from configuration wraps random data keys (DEKs); the
// wrapped DEKs live in the data_key table. Secrets are sealed with
// AES-256-GCM under a DEK, and the associated data binds each ciphertext to
// its table, column and row, so a ciphertext copied elsewhere does not open.
//
// Sealed format: version(1) | dek_id(4, big-endian) | nonce(12) | ciphertext+tag.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// KeySize is the size of the master key and of every data key.
const KeySize = 32

const (
	formatV1  = 1
	nonceSize = 12
	headerLen = 1 + 4 + nonceSize
)

var (
	// ErrDecrypt is returned when a ciphertext does not open: wrong key,
	// wrong associated data, or tampering. It deliberately carries no detail.
	ErrDecrypt = errors.New("secret: decryption failed")
	// ErrUnknownDEK is returned when a ciphertext names a DEK that is not
	// loaded in the keyring.
	ErrUnknownDEK = errors.New("secret: unknown data key")
	// ErrNoActiveDEK is returned by Seal when the keyring has no active DEK.
	ErrNoActiveDEK = errors.New("secret: no active data key")
)

// MasterKey is a key-encryption key.
type MasterKey struct {
	key [KeySize]byte
}

// ParseMasterKey decodes a base64 master key (standard or URL alphabet,
// padding optional), as produced by GenerateMasterKey.
func ParseMasterKey(s string) (MasterKey, error) {
	s = strings.TrimSpace(s)
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(s); err == nil {
			break
		}
	}
	if err != nil {
		return MasterKey{}, errors.New("secret: master key is not valid base64")
	}
	if len(raw) != KeySize {
		return MasterKey{}, fmt.Errorf("secret: master key must be %d bytes, got %d", KeySize, len(raw))
	}
	var mk MasterKey
	copy(mk.key[:], raw)
	return mk, nil
}

// GenerateMasterKey returns a new random master key in base64.
func GenerateMasterKey() (string, error) {
	var k [KeySize]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k[:]), nil
}

// ID returns a short, non-secret identifier of the master key: the first 8
// bytes of SHA-256 over a fixed label and the key, in hex. It is stored with
// each wrapped DEK so the controller can tell which master key wrapped it.
func (m MasterKey) ID() string {
	h := sha256.New()
	h.Write([]byte("opendectcell-controller kek id v1\x00"))
	h.Write(m.key[:])
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// String never reveals the key.
func (m MasterKey) String() string { return "MasterKey(" + m.ID() + ")" }

func (m MasterKey) aead() (cipher.AEAD, error) {
	b, err := aes.NewCipher(m.key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func wrapAD(dekID uint32) []byte {
	ad := []byte("opendectcell-controller dek v1\x00....")
	binary.BigEndian.PutUint32(ad[len(ad)-4:], dekID)
	return ad
}

// Wrap encrypts a data key under the master key. The DEK id is bound in as
// associated data, so a wrapped key cannot be moved to another id.
func (m MasterKey) Wrap(dekID uint32, dek []byte) ([]byte, error) {
	if len(dek) != KeySize {
		return nil, fmt.Errorf("secret: data key must be %d bytes", KeySize)
	}
	a, err := m.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize, nonceSize+KeySize+a.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return a.Seal(nonce, nonce, dek, wrapAD(dekID)), nil
}

// Unwrap reverses Wrap.
func (m MasterKey) Unwrap(dekID uint32, wrapped []byte) ([]byte, error) {
	a, err := m.aead()
	if err != nil {
		return nil, err
	}
	if len(wrapped) < nonceSize+a.Overhead() {
		return nil, ErrDecrypt
	}
	dek, err := a.Open(nil, wrapped[:nonceSize], wrapped[nonceSize:], wrapAD(dekID))
	if err != nil {
		return nil, ErrDecrypt
	}
	return dek, nil
}

// NewDEK returns a fresh random data key.
func NewDEK() ([]byte, error) {
	k := make([]byte, KeySize)
	_, err := rand.Read(k)
	return k, err
}

// Keyring holds the unwrapped data keys and seals and opens secrets. It is
// safe for concurrent use.
type Keyring struct {
	mu     sync.RWMutex
	keys   map[uint32]cipher.AEAD
	active uint32
	hasAct bool
}

// NewKeyring returns an empty keyring.
func NewKeyring() *Keyring { return &Keyring{keys: map[uint32]cipher.AEAD{}} }

// Add loads an unwrapped data key. If active is set, it becomes the key used
// by Seal.
func (k *Keyring) Add(id uint32, dek []byte, active bool) error {
	if len(dek) != KeySize {
		return fmt.Errorf("secret: data key must be %d bytes", KeySize)
	}
	b, err := aes.NewCipher(dek)
	if err != nil {
		return err
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[id] = a
	if active {
		k.active, k.hasAct = id, true
	}
	return nil
}

// Clone returns a keyring with the same keys and active key.
func (k *Keyring) Clone() *Keyring {
	k.mu.RLock()
	defer k.mu.RUnlock()
	c := &Keyring{keys: make(map[uint32]cipher.AEAD, len(k.keys)), active: k.active, hasAct: k.hasAct}
	for id, a := range k.keys {
		c.keys[id] = a
	}
	return c
}

// ActiveID returns the id of the active data key.
func (k *Keyring) ActiveID() (uint32, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.active, k.hasAct
}

// Location names where a secret is stored. It becomes the associated data,
// so a ciphertext opens only in the same table, column and row.
type Location struct {
	Table, Column, RowID string
}

func (l Location) ad() []byte {
	// Length-prefixed fields, so ("a","bc") and ("ab","c") differ.
	var b []byte
	b = append(b, "opendectcell-controller secret v1\x00"...)
	for _, s := range []string{l.Table, l.Column, l.RowID} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
		b = append(b, s...)
	}
	return b
}

// Seal encrypts plaintext for the given location under the active key.
func (k *Keyring) Seal(loc Location, plaintext []byte) ([]byte, error) {
	k.mu.RLock()
	id, ok := k.active, k.hasAct
	a := k.keys[id]
	k.mu.RUnlock()
	if !ok {
		return nil, ErrNoActiveDEK
	}
	out := make([]byte, headerLen, headerLen+len(plaintext)+a.Overhead())
	out[0] = formatV1
	binary.BigEndian.PutUint32(out[1:5], id)
	if _, err := rand.Read(out[5:headerLen]); err != nil {
		return nil, err
	}
	return a.Seal(out, out[5:headerLen], plaintext, append(loc.ad(), out[:5]...)), nil
}

// Open decrypts a value produced by Seal for the same location.
func (k *Keyring) Open(loc Location, sealed []byte) ([]byte, error) {
	if len(sealed) < headerLen || sealed[0] != formatV1 {
		return nil, ErrDecrypt
	}
	id := binary.BigEndian.Uint32(sealed[1:5])
	k.mu.RLock()
	a, ok := k.keys[id]
	k.mu.RUnlock()
	if !ok {
		return nil, ErrUnknownDEK
	}
	pt, err := a.Open(nil, sealed[5:headerLen], sealed[headerLen:], append(loc.ad(), sealed[:5]...))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// DEKID returns the data key id a sealed value was encrypted under, for
// rotation. It does not authenticate the value.
func DEKID(sealed []byte) (uint32, bool) {
	if len(sealed) < headerLen || sealed[0] != formatV1 {
		return 0, false
	}
	return binary.BigEndian.Uint32(sealed[1:5]), true
}
