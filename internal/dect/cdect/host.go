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
	"sync"
	"unsafe"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// The types that cross the boundary are package stack's, so the DECT core
// builds without cgo.
type (
	Mail       = stack.Mail
	Record     = stack.Record
	To         = stack.To
	Env        = stack.Env
	Config     = stack.Config
	Capability = stack.Capability

	Recycled = stack.Recycled
)

// What Recycle came to.
const (
	RecycleDone = stack.RecycleDone
	RecycleBusy = stack.RecycleBusy
	RecycleNo   = stack.RecycleNo
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

// contactsLoad serialises dect_contacts_load, which is not reentrant across stores.
var contactsLoad sync.Mutex

// Host is one cell's DECT base stack (stack.Stack). It is not safe for
// concurrent use: one goroutine per cell owns it.
type Host struct {
	c     *C.struct_cdect_host
	h     cgo.Handle
	env   Env
	cstrs []*C.char
	fault *Fault          // a panic caught in a callback, raised once C has returned
	dir   map[uint]string // the remote directory as set (SetDirectory)
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
	x.c.poll_signal = cbool(cfg.PollSignal)
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
	x.c.mask = C.uint16_t(cfg.AudioMask)
	x.c.codec = C.uint8_t(cfg.AudioCodec)
	b.cfg.defer_unknown = cbool(cfg.DeferUnknown)
	b.cfg.locate_validity = C.unsigned(cfg.LocateValidity)
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
	for k := range in.tpui {
		in.tpui[k] = C.uint8_t(r.TPUI[k])
	}
	C.cdect_host_record(x.c, C.unsigned(i))
	x.raise()
}

// AddFreeStore makes record i a place a new handset can enrol; tpui is the
// TPUI the handset gets (all zero: derived from the slot).
func (x *Host) AddFreeStore(i int, tpui [3]byte) {
	for k := range x.c.rec.tpui {
		x.c.rec.tpui[k] = C.uint8_t(tpui[k])
	}
	C.cdect_host_free_store(x.c, C.unsigned(i))
	x.raise()
}

// Resume answers the held request the Fetch callback was asked about: the
// record is in the table (AddRecord) or there is none (the handset is told
// its IPUI is unknown).
func (x *Host) Resume(now uint64) {
	C.cdect_host_resume(x.c, C.uint64_t(now))
	x.raise()
}

// Drop takes record i out of the table. False: not now (the base is not
// quiet) or there is no such record.
func (x *Host) Drop(i int) bool {
	ok := C.cdect_host_drop(x.c, C.unsigned(i)) == 0
	x.raise()
	return ok
}

// Recycle turns record i, which holds a subscription in one of the chip's
// slots, into a free store again, in place: the chip slot is cleared and tpui
// is the TPUI of the next handset to enrol there.
func (x *Host) Recycle(i int, tpui [3]byte) Recycled {
	for k := range x.c.rec.tpui {
		x.c.rec.tpui[k] = C.uint8_t(tpui[k])
	}
	rc := C.cdect_host_recycle(x.c, C.unsigned(i))
	x.raise()
	switch {
	case rc == 0:
		return RecycleDone
	case rc > 0:
		return RecycleBusy
	}
	return RecycleNo
}

// Quiet reports whether nothing is going on: no link, no call, nothing
// being set up.
func (x *Host) Quiet() bool { return C.cdect_host_quiet(x.c) != 0 }

// Signal is the field strength the chip last measured on a link to record i
// (dBm), and when, on the clock the host was given.
func (x *Host) Signal(i int) (dbm int, at uint64, ok bool) {
	var d C.int
	var t C.uint64_t
	if C.cdect_host_signal(x.c, C.unsigned(i), &d, &t) == 0 {
		return 0, 0, false
	}
	return int(d), uint64(t), true
}

// SetRFPI sets the RFPI the chip broadcasts (what the library takes from the
// chip's 0xF601 when it sees one: the first one wins, as there).
func (x *Host) SetRFPI(rfpi [5]byte) {
	if x.c.b.have_rfpi != 0 {
		return
	}
	for k := range rfpi {
		x.c.b.rfpi[k] = C.uint8_t(rfpi[k])
	}
	x.c.b.have_rfpi = 1
}

// HasRFPI: the stack knows what the chip broadcasts.
func (x *Host) HasRFPI() bool { return x.c.b.have_rfpi != 0 }

// SetName sets record i's name in the Internal Names List; false: the library
// refuses it (empty, or longer than 15 octets).
func (x *Host) SetName(i int, name string) bool {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	ok := C.cdect_host_set_name(x.c, C.unsigned(i), cs) == 0
	x.raise()
	return ok
}

// SetProfile gives record i the behaviour of a handset model, from the texts of
// its manufacturer's and its own profile file (either may be empty).
func (x *Host) SetProfile(i int, mfr, dev string) bool {
	var m, d *C.char
	if mfr != "" {
		m = C.CString(mfr)
		defer C.free(unsafe.Pointer(m))
	}
	if dev != "" {
		d = C.CString(dev)
		defer C.free(unsafe.Pointer(d))
	}
	return C.cdect_host_set_profile(x.c, C.unsigned(i), m, C.size_t(len(mfr)), d, C.size_t(len(dev))) == 0
}

// SetLocateValidity changes how long a location registration holds, in units
// of 40.96 s, for the links set up from now on; 0: no limit.
func (x *Host) SetLocateValidity(units uint) {
	x.c.b.cfg.locate_validity = C.unsigned(units)
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
	x.pollContacts()
}

// Tick runs the stack's timers; call it at least every 100 ms.
func (x *Host) Tick(now uint64) {
	C.cdect_host_tick(x.c, C.uint64_t(now))
	x.raise()
	x.pollContacts()
}

// SetContacts gives record i's handset its Contact List from a contacts file; false if
// the library refuses the text.
func (x *Host) SetContacts(i int, text string) bool {
	if i < 0 || text == "" {
		return false
	}
	// dect_contacts_load keeps the list it replaces in a static buffer: one load at a time in a process
	contactsLoad.Lock()
	defer contactsLoad.Unlock()
	cs := C.CString(text)
	defer C.free(unsafe.Pointer(cs))
	return C.cdect_host_set_contacts(x.c, C.unsigned(i), cs, C.size_t(len(text))) == 0
}

// SetCalls gives the base its call lists from a call lists file; false if the text is not one.
func (x *Host) SetCalls(text string) bool {
	if text == "" {
		return false
	}
	cs := C.CString(text)
	defer C.free(unsafe.Pointer(cs))
	return C.cdect_host_set_calls(x.c, cs, C.size_t(len(text))) == 0
}

// SetDirectory replaces the remote directory: the handsets of the same account on other cells.
func (x *Host) SetDirectory(entries []stack.DirEntry) bool {
	ok, changed := true, false
	keep := make(map[uint]bool, len(entries))
	for _, e := range entries {
		keep[e.ID] = true
		if name, have := x.dir[e.ID]; have && name == e.Name {
			continue
		}
		cs := C.CString(e.Name)
		if e.Name == "" || C.cdect_host_remote_set(x.c, C.unsigned(e.ID), cs) != 0 {
			ok = false
		} else {
			if x.dir == nil {
				x.dir = map[uint]string{}
			}
			x.dir[e.ID] = e.Name
			changed = true
		}
		C.free(unsafe.Pointer(cs))
	}
	for id := range x.dir {
		if !keep[id] {
			C.cdect_host_remote_set(x.c, C.unsigned(id), nil)
			delete(x.dir, id)
			changed = true
		}
	}
	if changed { // the handsets are told once
		C.cdect_host_names_refresh(x.c)
	}
	x.raise()
	return ok
}

// remoteName is the name the library holds for directory number id, "" if none (tests).
func (x *Host) remoteName(id uint) string {
	p := C.dect_base_remote_name(&x.c.b, C.unsigned(id))
	if p == nil {
		return ""
	}
	return C.GoString(p)
}

// callsMissed is how many unread missed calls terminal tid has on line (tests).
func (x *Host) callsMissed(tid, line int) int {
	return int(C.dect_calls_missed_unread(&x.c.calls, C.uint(tid), C.uint(line)))
}

// pollContacts reports what the handsets changed in their Contact Lists since the
// last call, one journal line at a time.
func (x *Host) pollContacts() {
	for i := range C.DECT_BASE_HS_MAX {
		if x.c.contacts[i] == nil {
			continue
		}
		for {
			n := C.cdect_host_contact_change(x.c, C.unsigned(i))
			if n == 0 {
				break
			}
			x.env.ContactsChanged(i, C.GoStringN(&x.c.out[0], C.int(n)))
		}
	}
}

// contactCount is how many contacts record i's list holds, -1 if it has none (tests).
func (x *Host) contactCount(i int) int {
	if i < 0 || i >= C.DECT_BASE_HS_MAX || x.c.contacts[i] == nil {
		return -1
	}
	return int(x.c.contacts[i].n)
}

// testContactPut puts a contact into record i's list as a handset would (tests).
func (x *Host) testContactPut(i int, name, digits string) bool {
	n, d := C.CString(name), C.CString(digits)
	defer C.free(unsafe.Pointer(n))
	defer C.free(unsafe.Pointer(d))
	return C.cdect_host_test_contact_put(x.c, C.unsigned(i), n, d) == 0
}

// Seen is when record i last had C-plane traffic, on the stack's clock; 0
// if it never has.
// Detached is when record i's handset last said it was switched off ({DETACH}), 0 if never; it
// is detached while that is not before Seen.
func (x *Host) Detached(i int) uint64 {
	if i < 0 {
		return 0
	}
	return uint64(C.cdect_host_last_detached(x.c, C.uint(i)))
}

func (x *Host) Seen(i int) uint64 {
	if i < 0 {
		return 0
	}
	return uint64(C.cdect_host_last_seen(x.c, C.uint(i)))
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
	for k := range rec.TPUI {
		rec.TPUI[k] = byte(r.tpui[k])
	}
	hostOf(h).env.Save(int(i), rec)
	clear(rec.UAK[:])
}

//export cdectFetch
func cdectFetch(h C.uintptr_t, id *C.uint8_t, n C.size_t) {
	defer catch(h, "fetch")
	hostOf(h).env.Fetch(goBytes(id, n))
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
	x := hostOf(h)
	var ng1, ng3, disp, tone C.uint8_t
	var prof [16]C.uint8_t
	var n C.size_t
	var c Capability
	if C.cdect_host_capability(x.c, i, &ng1, &ng3, &disp, &tone, &prof[0], C.size_t(len(prof)), &n) != 0 {
		c = Capability{NGPart1: ng1 != 0, NGPart3: ng3 != 0, Display: uint8(disp), Tone: uint8(tone)}
		for k := C.size_t(0); k < n; k++ {
			c.Profiles = append(c.Profiles, byte(prof[k]))
		}
	}
	x.env.CapLearned(int(i), c)
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
