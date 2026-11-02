// SPDX-License-Identifier: AGPL-3.0-or-later

// Package worker runs libopendectcell stacks in separate processes (ADR 0003): a
// crash or a hang in the C code ends one worker, not the controller. The
// controller keeps the DECT core; for each cell it holds a Remote stack, which
// implements stack.Stack by forwarding to a stack inside a worker, and the
// worker forwards the stack's callbacks (stack.Env) back.
//
// The channel is one SOCK_SEQPACKET socket pair per worker, so a message is one
// packet. Every message carries the id of the stack it is for.
package worker

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
)

// Message types. A reply to a request has type mRet and the request's number.
const (
	// controller -> worker
	mNew         uint8 = iota + 1 // gob(stack.Config), u64 core clock now
	mAddRecord                    // u32 i, gob(stack.Record)
	mAddFree                      // u32 i, tpui[3]
	mStart                        // request: -> mRet(error string)
	mRadioUp                      // u8
	mRegPINs                      // u32 n, n strings
	mRegWindow                    // u8 open, u32 secs, u64 now
	mMail                         // u8 src, u8 dst, u16 prim, bytes param, u64 now
	mHello                        // -
	mCtl                          // request: str line, u64 now, u8 isBridge -> mRet(u8)
	mResume                       // u64 now
	mDrop                         // request: u32 i -> mRet(u8)
	mRecycle                      // request: u32 i, tpui[3] -> mRet(u8)
	mSetName                      // request: u32 i, str -> mRet(u8)
	mSetProfile                   // request: u32 i, str, str -> mRet(u8)
	mSetRFPI                      // [5]
	mSetValidity                  // u32 units
	mSetContacts                  // request: u32 i, bytes text -> mRet(u8)
	mClose                        // request: -> mRet()
	mPing                         // -> mPong
	// worker -> controller
	mState                   // the snapshot, see state
	mEnvSend                 // request: mail -> mRet(str error)
	mEnvRoom                 // request: -> mRet(u8)
	mEnvLog                  // str
	mEnvEvent                // u8 to, str
	mEnvSave                 // u32 i, gob(stack.Record)
	mEnvFetch                // bytes ipui
	mEnvForget               // u32 i, str
	mEnvNamed                // u32 i, str
	mEnvNameVis              // request: u32 viewer, u32 slot -> mRet(u8)
	mEnvSwitched             // u32 cur, str
	mEnvRegisted             // u32 cur
	mEnvCapLearn             // u32 i, gob(stack.Capability)
	mEnvClock                // request: -> mRet(i64 unix nanos, u8 ok)
	mEnvMedia                // u8 handle, u8 channel, u8 wideband, u8 up
	mEnvContacts             // u32 i, str journal line
	mPong                    // -
	mGone                    // the worker's stack ended by itself (not used yet)
	mRet         uint8 = 200 // the answer to a request
)

// maxFrame is the largest message: a mail is at most 255 bytes of parameters, a
// record about 100, a contacts file (500 entries) about 100 KB.
const maxFrame = 128 << 10

const hdrLen = 1 + 4 + 4

// frame is one decoded message.
type frame struct {
	typ  uint8
	id   uint32 // the stack
	req  uint32 // the request number: 0 for a message that wants no answer
	body []byte
}

func (f frame) bytes() []byte {
	b := make([]byte, hdrLen, hdrLen+len(f.body))
	b[0] = f.typ
	binary.BigEndian.PutUint32(b[1:], f.id)
	binary.BigEndian.PutUint32(b[5:], f.req)
	return append(b, f.body...)
}

var errShort = errors.New("worker: short message")

func parseFrame(p []byte) (frame, error) {
	if len(p) < hdrLen {
		return frame{}, errShort
	}
	return frame{typ: p[0], id: binary.BigEndian.Uint32(p[1:]), req: binary.BigEndian.Uint32(p[5:]), body: p[hdrLen:]}, nil
}

// wbuf builds a message body.
type wbuf struct{ b []byte }

func (w *wbuf) u8(v uint8)   { w.b = append(w.b, v) }
func (w *wbuf) u16(v uint16) { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *wbuf) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *wbuf) u64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *wbuf) bool(v bool) {
	if v {
		w.u8(1)
	} else {
		w.u8(0)
	}
}
func (w *wbuf) bytes(p []byte) { w.u32(uint32(len(p))); w.b = append(w.b, p...) }
func (w *wbuf) str(s string)   { w.bytes([]byte(s)) }
func (w *wbuf) raw(p []byte)   { w.b = append(w.b, p...) }
func (w *wbuf) gob(v any) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		panic(fmt.Sprintf("worker: gob encode %T: %v", v, err)) // our own types: a programming error
	}
	w.bytes(buf.Bytes())
}

// rbuf reads a message body; the first error sticks and the rest read zeros.
type rbuf struct {
	b   []byte
	err error
}

func (r *rbuf) take(n int) []byte {
	if r.err != nil || n < 0 || len(r.b) < n {
		r.err = errShort
		return make([]byte, max(n, 0))[:0]
	}
	p := r.b[:n]
	r.b = r.b[n:]
	return p
}
func (r *rbuf) u8() uint8 {
	if p := r.take(1); len(p) == 1 {
		return p[0]
	}
	return 0
}
func (r *rbuf) u16() uint16 {
	if p := r.take(2); len(p) == 2 {
		return binary.BigEndian.Uint16(p)
	}
	return 0
}
func (r *rbuf) u32() uint32 {
	if p := r.take(4); len(p) == 4 {
		return binary.BigEndian.Uint32(p)
	}
	return 0
}
func (r *rbuf) u64() uint64 {
	if p := r.take(8); len(p) == 8 {
		return binary.BigEndian.Uint64(p)
	}
	return 0
}
func (r *rbuf) bool() bool { return r.u8() != 0 }
func (r *rbuf) bytes() []byte {
	n := r.u32()
	if n > maxFrame {
		r.err = errShort
		return nil
	}
	return append([]byte(nil), r.take(int(n))...)
}
func (r *rbuf) str() string { return string(r.bytes()) }
func (r *rbuf) gob(v any) {
	p := r.bytes()
	if r.err != nil {
		return
	}
	if err := gob.NewDecoder(bytes.NewReader(p)).Decode(v); err != nil {
		r.err = err
	}
}
