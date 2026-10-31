// SPDX-License-Identifier: AGPL-3.0-or-later

package cdect

// #include <stdlib.h>
// #include "host.h"
import "C"

import (
	"errors"
	"runtime/cgo"
	"time"
	"unsafe"
)

// Mail is one STUB mail, as MAIL_UP and MAIL_DOWN carry it
// (docs/cell-protocol.md §7.1). Never log the Param of the primitives in
// §7.3.
type Mail struct {
	Src, Dst uint8
	Prim     uint16
	Param    []byte
}

// Record is a handset record (libopendectcell's struct dect_hs_rec). UAK is the
// long-term key: store it encrypted (AGENTS.md §5), never log it.
type Record struct {
	Slot      uint
	UAK       [16]byte
	PortID    []byte // MM <<PORTABLE-IDENTITY>> content
	CCPortID  []byte // the identities an FP-originated {CC-SETUP} carries
	CCFixedID []byte
}

// To says where a bridge line goes (docs/bridge-interface.md §2).
type To int

const (
	ToAsker  To = C.CDECT_TO_ASKER  // the reply to the client that sent the verb
	ToBridge To = C.CDECT_TO_BRIDGE // call traffic, to the bridge
	ToAll    To = C.CDECT_TO_ALL    // `hs` lines, to every client
)

// Env is what one cell's stack needs from the controller. Every method runs
// on the goroutine that called into the Host, inside that call: it must not
// call back into the same Host.
type Env interface {
	// Send hands a mail to the cell (MAIL_DOWN). An error means it could
	// not be taken now.
	Send(m Mail) error
	// Room reports whether the tunnel can take one more C-plane mail;
	// LAPC frames leave the stack only then (hosting-dect-base.md §4, pump).
	Room() bool
	// Random fills b from a CSPRNG.
	Random(b []byte)
	// Log takes one line. Lines never carry keys; they do carry IPUIs.
	Log(line string)
	// Event takes one bridge line, newline included.
	Event(to To, line string)
	// Save stores record i, which changed.
	Save(i int, r Record)
	// Forget: record i is deregistered.
	Forget(i int, why string)
	// Named: record i was renamed to name.
	Named(i int, name string)
	// NameVisible: may the handset in chip slot viewer see slot's entry in
	// its Internal Names List (docs/list-access.md §1.1)?
	NameVisible(viewer, slot uint) bool
	// Switched: record cur is now the one the main link serves.
	Switched(cur int, why string)
	// Registered: record cur enrolled and is in the chip.
	Registered(cur int)
	// CapLearned: record i's capability is known.
	CapLearned(i int)
	// Clock is the time now and whether it is known to be right (NTP).
	Clock() (t time.Time, ok bool)
	// Media: the audio of the call on handle is bound to channel (up), or
	// released: MEDIA_OPEN and MEDIA_CLOSE (cell-protocol.md §8.1).
	Media(handle, channel uint8, wideband, up bool)
}

// Config is a cell's stack settings (libopendectcell's struct dect_base_cfg and
// struct dect_base_chip). The zero value is dect-stubd's default.
type Config struct {
	PIN          string // 4 to 8 decimal digits
	Narrowband   bool
	TwoCalls     bool
	NamesRO      bool // no handset renames (list-access.md §1.1)
	NoCipher     bool // a lab option: never in production
	NoLocAuth    bool
	BindFirst    bool
	AlertMs      uint
	DialWaitMs   uint // 0: 3000
	TimeEveryMin uint
	IPUIO        *uint32

	UAKTable     uint32 // from the cell's stub_version (hosting-dect-base.md §6)
	CheckDelayMs uint32
	Stub         string
}

// ErrNoSlot means no chip slot is left for a free store.
var ErrNoSlot = errors.New("cdect: no chip slot left")

// Host is one cell's DECT base stack. It is not safe for concurrent use:
// one goroutine per cell owns it.
type Host struct {
	c     *C.struct_cdect_host
	h     cgo.Handle
	env   Env
	cstrs []*C.char
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
	b.chip.uak_table = C.uint32_t(cfg.UAKTable)
	b.chip.check_delay_ms = C.uint32_t(cfg.CheckDelayMs)
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
}

// AddFreeStore makes record i a place a new handset can enrol.
func (x *Host) AddFreeStore(i int) {
	C.cdect_host_free_store(x.c, C.unsigned(i))
}

// Start finishes the set-up once the records are in.
func (x *Host) Start() error {
	if C.cdect_host_start(x.c) != 0 {
		return ErrNoSlot
	}
	return nil
}

// RadioUp tells the stack whether the cell's radio is on (STATUS).
func (x *Host) RadioUp(up bool) {
	x.c.radio_up = cbool(up)
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
}

// Tick runs the stack's timers; call it at least every 100 ms.
func (x *Host) Tick(now uint64) {
	C.cdect_host_tick(x.c, C.uint64_t(now))
}

// Hello sends a client that connected the `hello` and `hs` lines.
func (x *Host) Hello() {
	C.cdect_host_hello(x.c)
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
	return C.cdect_host_ctl(x.c, C.uint64_t(now), cbool(isBridge)) == C.CDECT_CTL_BRIDGE
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
func cdectSend(h C.uintptr_t, m *C.struct_dect_mail) C.int {
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
	return cbool(hostOf(h).env.Room())
}

//export cdectRandom
func cdectRandom(h C.uintptr_t, out *C.uint8_t, n C.size_t) {
	b := make([]byte, int(n))
	hostOf(h).env.Random(b)
	copyIn(unsafe.Slice(out, int(n)), b)
	clear(b)
}

//export cdectLog
func cdectLog(h C.uintptr_t, line *C.char) {
	hostOf(h).env.Log(C.GoString(line))
}

//export cdectEvent
func cdectEvent(h C.uintptr_t, to C.int, line *C.char, n C.size_t) {
	hostOf(h).env.Event(To(to), C.GoStringN(line, C.int(n)))
}

//export cdectSave
func cdectSave(h C.uintptr_t, i C.unsigned, r *C.struct_dect_hs_rec) {
	rec := Record{Slot: uint(r.slot),
		PortID:    goBytes(&r.port_id[0], r.port_id_len),
		CCPortID:  goBytes(&r.cc_port_id[0], r.cc_port_id_len),
		CCFixedID: goBytes(&r.cc_fixed_id[0], r.cc_fixed_id_len)}
	for k := range rec.UAK {
		rec.UAK[k] = byte(r.uak[k])
	}
	hostOf(h).env.Save(int(i), rec)
	clear(rec.UAK[:])
}

//export cdectForget
func cdectForget(h C.uintptr_t, i C.unsigned, why *C.char) {
	hostOf(h).env.Forget(int(i), C.GoString(why))
}

//export cdectNamed
func cdectNamed(h C.uintptr_t, i C.unsigned) {
	x := hostOf(h)
	x.env.Named(int(i), C.GoString(&x.c.b.name[i][0]))
}

//export cdectNameVisible
func cdectNameVisible(h C.uintptr_t, viewer, slot C.unsigned) C.int {
	return cbool(hostOf(h).env.NameVisible(uint(viewer), uint(slot)))
}

//export cdectSwitched
func cdectSwitched(h C.uintptr_t, cur C.unsigned, why *C.char) {
	hostOf(h).env.Switched(int(cur), C.GoString(why))
}

//export cdectRegistered
func cdectRegistered(h C.uintptr_t, cur C.unsigned) {
	hostOf(h).env.Registered(int(cur))
}

//export cdectCapLearned
func cdectCapLearned(h C.uintptr_t, i C.unsigned) {
	hostOf(h).env.CapLearned(int(i))
}

//export cdectClock
func cdectClock(h C.uintptr_t, t *C.int64_t, off *C.long) C.int {
	now, ok := hostOf(h).env.Clock()
	_, o := now.Zone()
	*t = C.int64_t(now.Unix())
	*off = C.long(o)
	return cbool(ok)
}

//export cdectMedia
func cdectMedia(h C.uintptr_t, handle, channel C.uint8_t, wideband, up C.int) {
	hostOf(h).env.Media(uint8(handle), uint8(channel), wideband != 0, up != 0)
}
