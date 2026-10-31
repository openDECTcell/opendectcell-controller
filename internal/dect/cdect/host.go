// SPDX-License-Identifier: AGPL-3.0-or-later

package cdect

/*
#include <stdlib.h>
#include "host.h"

// The registration window is the library's own call (dect_base.h); the
// reason is for its log line.
static inline void cdect_reg_window(struct cdect_host *c, int open, unsigned secs, uint64_t now)
{
	c->now = now;
	dect_base_reg_window(&c->b, open, secs, now, "the controller");
}
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime/cgo"
	"runtime/debug"
	"unsafe"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// The types that cross the boundary are package stack's, so the DECT core
// builds without cgo.
type (
	Mail   = stack.Mail
	Record = stack.Record
	To     = stack.To
	Env    = stack.Env
	Config = stack.Config
)

// Where a bridge line goes; the values are host.h's enum cdect_to.
const (
	ToAsker  = stack.ToAsker
	ToBridge = stack.ToBridge
	ToAll    = stack.ToAll
)

// The two numberings must agree: a mismatch fails to compile.
var (
	_ [stack.ToAsker - C.CDECT_TO_ASKER]struct{}
	_ [C.CDECT_TO_ASKER - stack.ToAsker]struct{}
	_ [stack.ToBridge - C.CDECT_TO_BRIDGE]struct{}
	_ [C.CDECT_TO_BRIDGE - stack.ToBridge]struct{}
	_ [stack.ToAll - C.CDECT_TO_ALL]struct{}
	_ [C.CDECT_TO_ALL - stack.ToAll]struct{}
)

// ErrNoSlot means no chip slot is left for a free store.
var ErrNoSlot = errors.New("cdect: no chip slot left")

// Host is one cell's DECT base stack (stack.Stack). It is not safe for
// concurrent use: one goroutine per cell owns it.
type Host struct {
	c     *C.struct_cdect_host
	h     cgo.Handle
	env   Env
	cstrs []*C.char
	fault *Fault // a panic caught in a callback, raised once C has returned
}

// Fault is a panic in one of the host's callbacks. It is caught there,
// because a panic must not unwind through C frames, and raised again by
// the Host method that called into C (libopendectcell-integration.md §8). The
// stack is then in an unknown state: close it.
type Fault struct {
	Callback string
	Value    any
	Stack    []byte
}

func (f *Fault) Error() string {
	return fmt.Sprintf("cdect: panic in callback %s: %v\n%s", f.Callback, f.Value, f.Stack)
}

// catch is deferred by every callback: it keeps the first panic for raise.
func catch(h C.uintptr_t, cb string) {
	v := recover()
	if v == nil {
		return
	}
	if x := hostOf(h); x.fault == nil {
		x.fault = &Fault{Callback: cb, Value: v, Stack: debug.Stack()}
	}
}

// raise panics with a fault caught during the last call into C.
func (x *Host) raise() {
	if f := x.fault; f != nil {
		x.fault = nil
		panic(f)
	}
}

// NewHost allocates a stack for one cell. Add the records, then Start.
func NewHost(env Env, cfg Config) *Host {
	x := &Host{env: env}
	x.h = cgo.NewHandle(x)
	x.c = C.cdect_host_new(C.uintptr_t(x.h))
	if x.c == nil {
		x.h.Delete()
		panic("cdect: out of C memory")
	}
	b := &x.c.b
	if len(cfg.PIN) <= C.DECT_PIN_DIGITS_MAX {
		for i := range len(cfg.PIN) {
			x.c.pin[i] = C.char(cfg.PIN[i])
		}
	}
	b.cfg.narrowband = cbool(cfg.Narrowband)
	b.cfg.two_calls = cbool(cfg.TwoCalls)
	b.cfg.names_readonly = cbool(cfg.NamesRO)
	b.cfg.no_cipher = cbool(cfg.NoCipher)
	b.cfg.locauth = cbool(!cfg.NoLocAuth)
	b.cfg.bind_first = cbool(cfg.BindFirst)
	b.cfg.alert_ms = C.unsigned(cfg.AlertMs)
	if cfg.DialWaitMs != 0 {
		b.cfg.dial_wait_ms = C.unsigned(cfg.DialWaitMs)
	}
	b.cfg.time_every_min = C.unsigned(cfg.TimeEveryMin)
	if cfg.IPUIO != nil {
		b.cfg.ipuio_set = 1
		b.cfg.ipuio = C.uint32_t(*cfg.IPUIO)
	}
	b.cfg.park_pli = C.uint(cfg.ParkPLI)
	b.chip.uak_table = C.uint32_t(cfg.UAKTable)
	b.chip.check_delay_ms = C.uint32_t(cfg.CheckDelayMs)
	x.c.no_dummy = cbool(cfg.NoDummyKey)
	if cfg.Stub != "" {
		s := C.CString(cfg.Stub)
		x.cstrs = append(x.cstrs, s)
		b.chip.stub = s
	}
	return x
}

// Close wipes and frees the stack. It is safe to call more than once.
func (x *Host) Close() {
	if x.c == nil {
		return
	}
	C.cdect_host_free(x.c)
	x.c = nil
	x.h.Delete()
	for _, s := range x.cstrs {
		C.free(unsafe.Pointer(s))
	}
	x.cstrs = nil
}

// AddRecord loads record i, a handset this cell serves.
func (x *Host) AddRecord(i int, r Record) {
	in := &x.c.rec
	in.slot = C.unsigned(r.Slot)
	copyIn(in.uak[:], r.UAK[:])
	in.port_id_len = C.size_t(copyCapped(in.port_id[:], r.PortID))
	in.cc_port_id_len = C.size_t(copyCapped(in.cc_port_id[:], r.CCPortID))
	in.cc_fixed_id_len = C.size_t(copyCapped(in.cc_fixed_id[:], r.CCFixedID))
	C.cdect_host_record(x.c, C.unsigned(i))
	x.raise()
}

// AddFreeStore makes record i a place a new handset can enrol.
func (x *Host) AddFreeStore(i int) {
	C.cdect_host_free_store(x.c, C.unsigned(i))
	x.raise()
}

// Start finishes the set-up once the records are in.
func (x *Host) Start() error {
	rc := C.cdect_host_start(x.c)
	x.raise()
	if rc != 0 {
		return ErrNoSlot
	}
	return nil
}

// RadioUp tells the stack whether the cell's radio is on (STATUS).
func (x *Host) RadioUp(up bool) {
	x.c.radio_up = cbool(up)
}

// SetRegPINs sets the registration PINs by index, "" for an unused one
// (openDECTcell API 139). A link already set up keeps the list it was given.
func (x *Host) SetRegPINs(pins []string) {
	n := min(len(pins), C.DECT_MM_REG_PINS_MAX)
	for i := range x.c.reg_pin {
		clear(x.c.reg_pin[i][:])
		x.c.reg_pin_ptr[i] = &x.c.reg_pin[i][0]
		if i < n && len(pins[i]) <= C.DECT_PIN_DIGITS_MAX {
			for k := range len(pins[i]) {
				x.c.reg_pin[i][k] = C.char(pins[i][k])
			}
		}
	}
	x.c.b.cfg.reg_pins = &x.c.reg_pin_ptr[0]
	x.c.b.cfg.n_reg_pins = C.unsigned(n)
}

// RegWindow opens registration for secs (0: until closed), or closes it.
func (x *Host) RegWindow(open bool, secs uint, now uint64) {
	C.cdect_reg_window(x.c, cbool(open), C.unsigned(secs), C.uint64_t(now))
	x.raise()
}

// RegState: registration is open; a free record can take a new handset.
func (x *Host) RegState() (open, free bool) {
	return x.c.b.reg_open != 0, C.dect_base_free_record(&x.c.b) >= 0
}

// Mail takes one mail from the chip (MAIL_UP); now is the controller's
// monotonic clock in milliseconds.
func (x *Host) Mail(m Mail, now uint64) {
	if len(m.Param) > C.DECT_MAIL_PARAM_MAX {
		return
	}
	in := &x.c.in
	in.src = C.uint8_t(m.Src)
	in.dst = C.uint8_t(m.Dst)
	in.prim = C.uint16_t(m.Prim)
	in.param_len = C.uint16_t(len(m.Param))
	copyIn(in.param[:], m.Param)
	C.cdect_host_mail(x.c, C.uint64_t(now))
	x.raise()
}

// Tick runs the stack's timers; call it at least every 100 ms.
func (x *Host) Tick(now uint64) {
	C.cdect_host_tick(x.c, C.uint64_t(now))
	x.raise()
}

// Hello sends a client that connected the `hello` and `hs` lines.
func (x *Host) Hello() {
	C.cdect_host_hello(x.c)
	x.raise()
}

// Ctl runs one bridge-interface line (without its newline) from a client;
// isBridge says that client is the bridge. It reports whether the client
// has just become the bridge.
func (x *Host) Ctl(line string, now uint64, isBridge bool) bool {
	if len(line) > C.DECT_CTL_LINE_MAX {
		line = line[:C.DECT_CTL_LINE_MAX]
	}
	for i := range len(line) {
		x.c.line[i] = C.char(line[i])
	}
	x.c.line[len(line)] = 0
	rc := C.cdect_host_ctl(x.c, C.uint64_t(now), cbool(isBridge))
	x.raise()
	return rc == C.CDECT_CTL_BRIDGE
}

func cbool(v bool) C.int {
	if v {
		return 1
	}
	return 0
}

func copyCapped(dst []C.uint8_t, src []byte) int {
	n := min(len(dst), len(src))
	copyIn(dst, src[:n])
	return n
}

func hostOf(h C.uintptr_t) *Host {
	return cgo.Handle(h).Value().(*Host)
}

func goBytes(p *C.uint8_t, n C.size_t) []byte {
	return C.GoBytes(unsafe.Pointer(p), C.int(n))
}

//export cdectSend
func cdectSend(h C.uintptr_t, m *C.struct_dect_mail) (rc C.int) {
	rc = -1
	defer catch(h, "send")
	x := hostOf(h)
	err := x.env.Send(Mail{Src: uint8(m.src), Dst: uint8(m.dst), Prim: uint16(m.prim),
		Param: goBytes(&m.param[0], C.size_t(m.param_len))})
	if err != nil {
		return -1
	}
	return 0
}

//export cdectRoom
func cdectRoom(h C.uintptr_t) C.int {
	defer catch(h, "room")
	return cbool(hostOf(h).env.Room())
}

//export cdectRandom
func cdectRandom(h C.uintptr_t, out *C.uint8_t, n C.size_t) {
	defer catch(h, "random")
	b := make([]byte, int(n))
	hostOf(h).env.Random(b)
	copyIn(unsafe.Slice(out, int(n)), b)
	clear(b)
}

//export cdectLog
func cdectLog(h C.uintptr_t, line *C.char) {
	defer catch(h, "log")
	hostOf(h).env.Log(C.GoString(line))
}

//export cdectEvent
func cdectEvent(h C.uintptr_t, to C.int, line *C.char, n C.size_t) {
	defer catch(h, "event")
	hostOf(h).env.Event(To(to), C.GoStringN(line, C.int(n)))
}

//export cdectSave
func cdectSave(h C.uintptr_t, i C.unsigned, r *C.struct_dect_hs_rec) {
	defer catch(h, "save")
	rec := Record{Slot: uint(r.slot),
		PortID:    goBytes(&r.port_id[0], r.port_id_len),
		CCPortID:  goBytes(&r.cc_port_id[0], r.cc_port_id_len),
		CCFixedID: goBytes(&r.cc_fixed_id[0], r.cc_fixed_id_len),
		RegPIN1:   uint(r.reg_pin1)}
	for k := range rec.UAK {
		rec.UAK[k] = byte(r.uak[k])
	}
	hostOf(h).env.Save(int(i), rec)
	clear(rec.UAK[:])
}

//export cdectForget
func cdectForget(h C.uintptr_t, i C.unsigned, why *C.char) {
	defer catch(h, "forget")
	hostOf(h).env.Forget(int(i), C.GoString(why))
}

//export cdectNamed
func cdectNamed(h C.uintptr_t, i C.unsigned) {
	defer catch(h, "named")
	x := hostOf(h)
	x.env.Named(int(i), C.GoString(&x.c.b.name[i][0]))
}

//export cdectNameVisible
func cdectNameVisible(h C.uintptr_t, viewer, slot C.unsigned) C.int {
	defer catch(h, "namevisible")
	return cbool(hostOf(h).env.NameVisible(uint(viewer), uint(slot)))
}

//export cdectSwitched
func cdectSwitched(h C.uintptr_t, cur C.unsigned, why *C.char) {
	defer catch(h, "switched")
	hostOf(h).env.Switched(int(cur), C.GoString(why))
}

//export cdectRegistered
func cdectRegistered(h C.uintptr_t, cur C.unsigned) {
	defer catch(h, "registered")
	hostOf(h).env.Registered(int(cur))
}

//export cdectCapLearned
func cdectCapLearned(h C.uintptr_t, i C.unsigned) {
	defer catch(h, "caplearned")
	hostOf(h).env.CapLearned(int(i))
}

//export cdectClock
func cdectClock(h C.uintptr_t, dt *C.struct_dect_time, off *C.long) C.int {
	defer catch(h, "clock")
	*dt, *off = C.struct_dect_time{}, 0 // what C reads if Clock panics
	now, ok := hostOf(h).env.Clock()
	_, o := now.Zone()
	*dt = C.struct_dect_time{year: C.uint8_t(now.Year() % 100), month: C.uint8_t(now.Month()),
		day: C.uint8_t(now.Day()), hour: C.uint8_t(now.Hour()), minute: C.uint8_t(now.Minute()),
		second: C.uint8_t(now.Second()), tz_quarters: C.int16_t(o / 900), tz_valid: 1}
	*off = C.long(o)
	return cbool(ok)
}

//export cdectMedia
func cdectMedia(h C.uintptr_t, handle, channel C.uint8_t, wideband, up C.int) {
	defer catch(h, "media")
	hostOf(h).env.Media(uint8(handle), uint8(channel), wideband != 0, up != 0)
}

var _ stack.Stack = (*Host)(nil)

// NewStack is NewHost as a stack.New, for the DECT core.
func NewStack(env stack.Env, cfg stack.Config) stack.Stack {
	return NewHost(env, cfg)
}
