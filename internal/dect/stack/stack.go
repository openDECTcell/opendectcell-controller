// SPDX-License-Identifier: AGPL-3.0-or-later

// Package stack names one cell's DECT base stack without cgo: the types that
// cross between it and the controller, and the interface the DECT core
// drives it through. internal/dect/cdect implements it over libopendectcell; the
// core (internal/dect/core) uses only this package, so everything but cdect
// builds and tests without a C compiler or the openDECTcell submodule.
package stack

import "time"

// Mail is one STUB mail, as MAIL_UP and MAIL_DOWN carry it
// (docs/cell-protocol.md §7.1). Never log the Param of the primitives in
// §7.3.
type Mail struct {
	Src, Dst uint8
	Prim     uint16
	Param    []byte
}

// Record is a handset record (libopendectcell's struct dect_hs_rec). UAK is the
// long-term key: store it encrypted (AGENTS.md §5), never log it. Slot is
// where the record sits in this cell's chip, not what the handset is
// (openDECTcell docs/more-than-six.md).
type Record struct {
	Slot      uint
	UAK       [16]byte
	PortID    []byte // MM <<PORTABLE-IDENTITY>> content
	CCPortID  []byte // the identities an FP-originated {CC-SETUP} carries
	CCFixedID []byte
	// TPUI is the temporary identity the handset holds, {byte 0, 0x11,
	// byte 2}; all zero lets the stack derive it from Slot (a legacy
	// record). A pool handset's TPUI is the same on every cell.
	TPUI [3]byte
	// RegPIN1 is 1 + the index of the registration PIN the handset proved,
	// for a record a registration created in this run; 0 otherwise.
	RegPIN1 uint
}

// Capability is what a handset said of itself in <<TERMINAL-CAPABILITY>> (EN 300
// 175-5 7.7.41), as received.
type Capability struct {
	NGPart1, NGPart3 bool   // NG-DECT Part 1 (wideband), Part 3 (CAT-iq)
	Display, Tone    uint8  // the codes of the display and tone capabilities
	Profiles         []byte // the profile indicator octets
}

// Recycled is what Stack.Recycle came to.
type Recycled int

const (
	RecycleDone Recycled = iota // the record is a free store again
	RecycleBusy                 // the stack is not quiet: try again later
	RecycleNo                   // not a record with a chip slot
)

// To says where a bridge line goes (docs/bridge-interface.md §2).
type To int

const (
	ToAsker  To = iota // the reply to the client that sent the verb
	ToBridge           // call traffic, to the bridge
	ToAll              // `hs` lines, to every client
)

// Env is what one cell's stack needs from the controller. Every method runs
// on the goroutine that called into the Stack, inside that call: it must
// not call back into the same Stack.
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
	// Fetch: a handset this cell holds no record of asked to locate or call
	// (Config.DeferUnknown); ipui is its identity. The host answers, from
	// anywhere, with Stack.AddRecord (if it knows the handset) and
	// Stack.Resume, within about 15 s.
	Fetch(ipui []byte)
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
	// CapLearned: record i's handset has said what it is (its terminal
	// capability).
	CapLearned(i int, c Capability)
	// Clock is the time now and whether it is known to be right (NTP).
	Clock() (t time.Time, ok bool)
	// Media: the audio of the call on handle is bound to channel (up), or
	// released: MEDIA_OPEN and MEDIA_CLOSE (cell-protocol.md §8.1).
	Media(handle, channel uint8, wideband, up bool)
}

// Config is a cell's stack settings (libopendectcell's struct dect_base_cfg and
// struct dect_base_chip). The zero value is dect-stubd's default.
type Config struct {
	PIN        string // 4 to 8 decimal digits
	Narrowband bool
	TwoCalls   bool
	NamesRO    bool // no handset renames (list-access.md §1.1)
	// PollSignal asks the chip once a second for the field strength of a link
	// while a handset is connected (Stack.Signal).
	PollSignal   bool
	NoCipher     bool // a lab option: never in production
	NoLocAuth    bool
	BindFirst    bool
	AlertMs      uint
	DialWaitMs   uint // 0: 3000
	TimeEveryMin uint
	IPUIO        *uint32
	// DeferUnknown holds a locate or call from a handset the cell holds no
	// record of until the host has answered Env.Fetch.
	DeferUnknown bool
	// LocateValidity is the location registration's validity in units of
	// 40.96 s (the handset registers again after it); 0: none.
	LocateValidity uint
	// ParkPLI is the PARK length handsets are given (class B: 19, ARC and
	// EIC); 0 is the class's whole ARI.
	ParkPLI uint

	UAKTable     uint32 // from the cell's stub_version (hosting-dect-base.md §6)
	CheckDelayMs uint32
	Stub         string
	// NoDummyKey: the golden traces replay dect-stubd, which writes no
	// dummy key into a chip table that would otherwise be empty.
	NoDummyKey bool
}

// Stack is one cell's base stack. It is not safe for concurrent use: one
// goroutine owns it. now is a monotonic clock in milliseconds.
type Stack interface {
	// AddRecord loads record i, a handset this cell serves.
	AddRecord(i int, r Record)
	// AddFreeStore makes record i a place a new handset can enrol, with
	// tpui as its TPUI (all zero: derived from the slot).
	AddFreeStore(i int, tpui [3]byte)
	// Start finishes the set-up once the records are in.
	Start() error
	// RadioUp: the cell's radio is on; the records go into the chip.
	RadioUp(up bool)
	// SetRegPINs sets the registration PINs by index, "" for an unused
	// one; the next link set up uses them (adr/0005-registration-flow.md).
	SetRegPINs(pins []string)
	// RegWindow opens registration for secs (0: until closed), or closes it.
	RegWindow(open bool, secs uint, now uint64)
	// RegState: registration is open; a free record can take a new handset.
	RegState() (open, free bool)
	// Mail takes one mail from the chip (MAIL_UP).
	Mail(m Mail, now uint64)
	// Tick runs the stack's timers; call it at least every 100 ms.
	Tick(now uint64)
	// Seen is when record i last had C-plane traffic (any message of its
	// handset on any link), on the clock Tick is given; 0 if never.
	Seen(i int) uint64
	// Hello sends a client that connected the `hello` and `hs` lines.
	Hello()
	// Ctl runs one bridge-interface line; it reports whether the client
	// has just become the bridge.
	Ctl(line string, now uint64, isBridge bool) bool
	// Resume answers the request Env.Fetch reported: the record is in the
	// table, or there is none and the handset is told its IPUI is unknown.
	Resume(now uint64)
	// Drop takes record i out of the table. False: the stack is not idle,
	// try again later.
	Drop(i int) bool
	// Recycle turns record i, which holds a subscription in a chip slot, into a
	// free store again in place: the slot is cleared in the chip and tpui is the
	// TPUI of the next handset to enrol there.
	Recycle(i int, tpui [3]byte) Recycled
	// Quiet reports whether the stack is idle: no link, no call, nothing
	// being set up.
	Quiet() bool
	// Signal is the field strength the chip last measured on a link to record
	// i, in dBm, and when (the clock Tick is given); ok is false if none was.
	Signal(i int) (dbm int, at uint64, ok bool)
	// SetRFPI gives the stack the RFPI its chip broadcasts. A registering
	// handset is given it as its base's PARK, and ringing a handset names the
	// base with it: without it a handset registers against an all-zero base
	// and can never find the base again after a reboot.
	SetRFPI(rfpi [5]byte)
	// HasRFPI: the stack knows the RFPI (SetRFPI, or the chip's 0xF601).
	HasRFPI() bool
	// SetName sets record i's name in the Internal Names List. False: the
	// stack refuses it (empty, or longer than 15 octets).
	SetName(i int, name string) bool
	// SetProfile gives record i the behaviour of a handset model, from the
	// texts of its manufacturer's and its own profile file (either may be
	// empty: the standard behaviour). False: the stack cannot read them.
	SetProfile(i int, mfr, dev string) bool
	// SetLocateValidity sets the location validity in units of 40.96 s for
	// the links from now on; 0: none.
	SetLocateValidity(units uint)
	// Close wipes and frees the stack.
	Close()
}

// New makes a stack for one cell; nil where the binary has no libopendectcell.
type New func(env Env, cfg Config) Stack
