// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wire implements the framing and message encodings of the
// cell↔controller protocol (docs/cell-protocol.md §3). It has no I/O policy
// of its own: it reads and writes frames and encodes and decodes bodies.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxBody is the largest body of any frame (§3.1).
const MaxBody = 4096

// Type is a message type (§3.3).
type Type uint16

// The message types of §3.3. The high byte groups them: 0x00 link,
// 0x01 STUB tunnel, 0x02 cell management, 0x03 media; 0x04xx-0x05xx and
// 0x0Fxx are reserved (§12) and not defined here. "up" is cell to
// controller, "down" the reverse.
const (
	Hello          Type = 0x0001 // up, §6.1: first frame of a session
	HelloAck       Type = 0x0002 // down, §6.1: answer to Hello, carries timers and state
	Heartbeat      Type = 0x0003 // both, §6.3: 4-octet sender time
	HeartbeatEcho  Type = 0x0004 // both, §6.3: the Heartbeat body sent back
	Goodbye        Type = 0x0005 // both, §11: orderly close with a reason
	Error          Type = 0x0006 // both, §11: a frame was refused, the link stays
	Flow           Type = 0x0007 // up, §9.3: the cell's chip queue state
	EnrollRequest  Type = 0x0010 // up, §5.2: unauthenticated connections only
	EnrollResponse Type = 0x0011 // down, §5.2
	MailUp         Type = 0x0100 // up, §7.1: one STUB mail from the chip
	MailDown       Type = 0x0101 // down, §7.1: one STUB mail for the chip
	MailReject     Type = 0x0102 // up, §7.2: a Mail Down could not reach the chip
	CellState      Type = 0x0200 // down, §6.2: pending, active, disabled or revoked
	ConfigSet      Type = 0x0201 // down, §10: settings, so far only the RFPI
	ConfigAck      Type = 0x0202 // up, §10: answer to ConfigSet
	Status         Type = 0x0203 // up, §10: radio and chip state
	StatusRequest  Type = 0x0204 // down, §10: asks for Status; empty body
	StatsRequest   Type = 0x0205 // down, §10: asks for Stats; empty body
	Stats          Type = 0x0206 // up, §10: counters
	StateSnapshot  Type = 0x0207 // up, §6.4: the cell's state for a resume
	RenewRequest   Type = 0x0210 // up, §5.4: CSR for a new certificate
	RenewResponse  Type = 0x0211 // down, §5.4
	TrustUpdate    Type = 0x0212 // down, §5.6: new CA during rotation
	TrustAck       Type = 0x0213 // up, §5.6
	MediaOpen      Type = 0x0300 // down, §8.1: start an audio flow
	MediaOpenAck   Type = 0x0301 // up, §8.1
	MediaClose     Type = 0x0302 // down, §8.1: end an audio flow
	MediaClosed    Type = 0x0303 // up, §8.1: a flow ended, asked for or not
)

// Class is a send priority (§9.1).
type Class uint8

// The send classes: each side sends with strict priority, P0 before P1
// before P2 (§9.1).
const (
	P0 Class = iota // urgent: heartbeat, mail, media control
	P1              // management: handshake, state, status
	P2              // bulk: statistics
)

// Info is a registry entry: the type's class and body bound.
type Info struct {
	Name    string
	Class   Class
	MaxBody int
}

var registry = map[Type]Info{
	Hello: {"HELLO", P1, 1024}, HelloAck: {"HELLO_ACK", P1, 512},
	Heartbeat: {"HEARTBEAT", P0, 4}, HeartbeatEcho: {"HEARTBEAT_ECHO", P0, 4},
	Goodbye: {"GOODBYE", P1, 256}, Error: {"ERROR", P1, 256}, Flow: {"FLOW", P0, 3},
	EnrollRequest: {"ENROLL_REQUEST", P1, 2048}, EnrollResponse: {"ENROLL_RESPONSE", P1, 4096},
	MailUp: {"MAIL_UP", P0, 258}, MailDown: {"MAIL_DOWN", P0, 254}, MailReject: {"MAIL_REJECT", P0, 3},
	CellState: {"CELL_STATE", P1, 256}, ConfigSet: {"CONFIG_SET", P1, 512}, ConfigAck: {"CONFIG_ACK", P1, 64},
	Status: {"STATUS", P1, 512}, StatusRequest: {"STATUS_REQUEST", P1, 0}, StatsRequest: {"STATS_REQUEST", P1, 0},
	Stats: {"STATS", P2, 2048}, StateSnapshot: {"STATE_SNAPSHOT", P1, 1024},
	RenewRequest: {"RENEW_REQUEST", P1, 2048}, RenewResponse: {"RENEW_RESPONSE", P1, 4096},
	TrustUpdate: {"TRUST_UPDATE", P1, 4096}, TrustAck: {"TRUST_ACK", P1, 64},
	MediaOpen: {"MEDIA_OPEN", P0, 512}, MediaOpenAck: {"MEDIA_OPEN_ACK", P0, 64},
	MediaClose: {"MEDIA_CLOSE", P0, 64}, MediaClosed: {"MEDIA_CLOSED", P0, 64},
}

// Lookup returns the registry entry for t.
func Lookup(t Type) (Info, bool) {
	i, ok := registry[t]
	return i, ok
}

func (t Type) String() string {
	if i, ok := registry[t]; ok {
		return i.Name
	}
	return fmt.Sprintf("0x%04x", uint16(t))
}

// Frame is one message on the link.
type Frame struct {
	Type Type
	Corr uint16
	Body []byte
}

// ErrFraming means the stream is no longer framed; the link must close.
var ErrFraming = errors.New("wire: framing lost")

// ReadFrame reads one frame. A length outside 4..4100 returns ErrFraming
// (§3.4); the body is not checked against its type's bound here.
func ReadFrame(r io.Reader) (Frame, error) {
	var h [6]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Frame{}, err
	}
	n := int(binary.BigEndian.Uint16(h[0:2]))
	if n < 4 || n > 4+MaxBody {
		return Frame{}, fmt.Errorf("%w: length %d", ErrFraming, n)
	}
	f := Frame{Type: Type(binary.BigEndian.Uint16(h[2:4])), Corr: binary.BigEndian.Uint16(h[4:6])}
	if n > 4 {
		f.Body = make([]byte, n-4)
		if _, err := io.ReadFull(r, f.Body); err != nil {
			return Frame{}, err
		}
	}
	return f, nil
}

// Append encodes f onto b.
func (f Frame) Append(b []byte) ([]byte, error) {
	if len(f.Body) > MaxBody {
		return b, fmt.Errorf("wire: %s body of %d bytes exceeds %d", f.Type, len(f.Body), MaxBody)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(4+len(f.Body)))
	b = binary.BigEndian.AppendUint16(b, uint16(f.Type))
	b = binary.BigEndian.AppendUint16(b, f.Corr)
	return append(b, f.Body...), nil
}

// CheckBound reports a body larger than its known type allows. Unknown types
// pass; the caller decides about them (§3.4).
func (f Frame) CheckBound() error {
	if i, ok := registry[f.Type]; ok && len(f.Body) > i.MaxBody {
		return fmt.Errorf("wire: %s body %d > %d", f.Type, len(f.Body), i.MaxBody)
	}
	return nil
}

// TLV is a decoded TLV body (§3.2). Repeated tags keep every value in order;
// Get returns the last, as the spec requires for non-repeatable tags.
type TLV map[uint8][][]byte

// ErrMalformed means a body does not match its layout.
var ErrMalformed = errors.New("wire: malformed body")

// ParseTLV decodes a TLV body.
func ParseTLV(b []byte) (TLV, error) {
	t := TLV{}
	for len(b) > 0 {
		if len(b) < 3 {
			return nil, fmt.Errorf("%w: truncated TLV header", ErrMalformed)
		}
		tag, n := b[0], int(binary.BigEndian.Uint16(b[1:3]))
		b = b[3:]
		if n > len(b) {
			return nil, fmt.Errorf("%w: tag %d length %d beyond body", ErrMalformed, tag, n)
		}
		t[tag] = append(t[tag], b[:n:n])
		b = b[n:]
	}
	return t, nil
}

// Get returns the last value of tag.
func (t TLV) Get(tag uint8) ([]byte, bool) {
	v := t[tag]
	if len(v) == 0 {
		return nil, false
	}
	return v[len(v)-1], true
}

// All returns every value of a repeatable tag.
func (t TLV) All(tag uint8) [][]byte { return t[tag] }

// Builder encodes a TLV body.
type Builder struct{ b []byte }

// Bytes adds a tag with raw bytes. Values over 65535 bytes are a programming
// error and panic.
func (w *Builder) Bytes(tag uint8, v []byte) *Builder {
	if len(v) > 0xFFFF {
		panic("wire: TLV value too long")
	}
	w.b = append(w.b, tag)
	w.b = binary.BigEndian.AppendUint16(w.b, uint16(len(v)))
	w.b = append(w.b, v...)
	return w
}

// String adds a tag whose value is the bytes of s, without a terminator.
func (w *Builder) String(tag uint8, s string) *Builder { return w.Bytes(tag, []byte(s)) }

// U8 adds a one-octet integer tag.
func (w *Builder) U8(tag uint8, v uint8) *Builder { return w.Bytes(tag, []byte{v}) }

// U16 adds a two-octet integer tag, big endian like every multi-octet field (§3.2).
func (w *Builder) U16(tag uint8, v uint16) *Builder {
	return w.Bytes(tag, binary.BigEndian.AppendUint16(nil, v))
}

// U32 adds a four-octet big-endian integer tag.
func (w *Builder) U32(tag uint8, v uint32) *Builder {
	return w.Bytes(tag, binary.BigEndian.AppendUint32(nil, v))
}

// U64 adds an eight-octet big-endian integer tag.
func (w *Builder) U64(tag uint8, v uint64) *Builder {
	return w.Bytes(tag, binary.BigEndian.AppendUint64(nil, v))
}

// Build returns the encoded body.
func (w *Builder) Build() []byte { return w.b }

// Typed accessors. Each returns ErrMalformed for a wrong length and, for
// required tags, for a missing tag.

func need(t TLV, tag uint8, n int) ([]byte, error) {
	v, ok := t.Get(tag)
	if !ok {
		return nil, fmt.Errorf("%w: required tag %d missing", ErrMalformed, tag)
	}
	if n >= 0 && len(v) != n {
		return nil, fmt.Errorf("%w: tag %d is %d bytes, want %d", ErrMalformed, tag, len(v), n)
	}
	return v, nil
}

// U8 returns a required one-octet tag; a missing tag or another length is ErrMalformed.
func (t TLV) U8(tag uint8) (uint8, error) {
	v, err := need(t, tag, 1)
	if err != nil {
		return 0, err
	}
	return v[0], nil
}

// U16 returns a required two-octet big-endian tag; a missing tag or another length is ErrMalformed.
func (t TLV) U16(tag uint8) (uint16, error) {
	v, err := need(t, tag, 2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(v), nil
}

// U32 returns a required four-octet big-endian tag; a missing tag or another length is ErrMalformed.
func (t TLV) U32(tag uint8) (uint32, error) {
	v, err := need(t, tag, 4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(v), nil
}

// U64 returns a required eight-octet big-endian tag; a missing tag or another length is ErrMalformed.
func (t TLV) U64(tag uint8) (uint64, error) {
	v, err := need(t, tag, 8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(v), nil
}

// Fixed returns a required tag of exactly n bytes.
func (t TLV) Fixed(tag uint8, n int) ([]byte, error) { return need(t, tag, n) }

// Str returns a required string tag of at most maxLen bytes.
func (t TLV) Str(tag uint8, maxLen int) (string, error) {
	v, err := need(t, tag, -1)
	if err != nil {
		return "", err
	}
	if len(v) > maxLen {
		return "", fmt.Errorf("%w: tag %d longer than %d", ErrMalformed, tag, maxLen)
	}
	return string(v), nil
}

// OptStr returns an optional string tag of at most maxLen bytes ("" if absent).
func (t TLV) OptStr(tag uint8, maxLen int) (string, error) {
	if _, ok := t.Get(tag); !ok {
		return "", nil
	}
	return t.Str(tag, maxLen)
}

// Has reports whether tag is present.
func (t TLV) Has(tag uint8) bool { _, ok := t.Get(tag); return ok }
