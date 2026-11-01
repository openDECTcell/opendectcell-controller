// SPDX-License-Identifier: AGPL-3.0-or-later

package cdect

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The golden traces of openDECTcell (tests/stubd-traces/, format at the top of
// tests/test_stubd.c) replayed through this package's host: the same
// actions in, the same mails and bridge lines out, is the same base
// (../opendectcell/docs/hosting-dect-base.md §8). The traces were recorded
// against dect-stubd, so two kinds of its output are not the controller's
// and are skipped (skipLine).

const traceDir = "../../../third_party/opendectcell/tests/stubd-traces"

// stubdStartMails is dect-stubd's radio bring-up, sent before anything
// else; on the controller the cell brings its radio up (cell-protocol.md).
var stubdStartMails = map[string]bool{
	"mail 0153 00": true, "mail 0169 07": true, "mail 0157 00": true,
	"mail 015b 01": true, "mail 022c 01": true,
}

// skipLine reports an expected line that only dect-stubd produces: its
// start-up mails (in the start block only), the chip session's contact
// mail 0xF600 (the cell keeps the session), and the audio socket's `audio`
// event, which the controller replaces with MEDIA_OPEN.
func skipLine(line string, atStart bool) bool {
	return (atStart && stubdStartMails[line]) || line == "mail f600" ||
		strings.HasPrefix(line, "ev audio ")
}

// onePassEarly lists actions after which this host sends the next action's
// first expected line already. dect-stubd reads its control socket in the
// middle of a loop pass, after the deregistration step, so its page for a
// `deregister` goes out one pass (10 ms) later; this host runs a command
// before dect_base_tick(), which has no point for one in between.
var onePassEarly = map[string]map[int]bool{
	"deregister": {27: true},
}

var apiRE = regexp.MustCompile(` api=\d+`)

// traceEnv stands in for the chip, the handsets and the bridge's client.
type traceEnv struct {
	t       *testing.T
	host    *Host
	now     uint64
	wall    int64
	rnd     int
	bridge  bool
	pps     map[uint8]*PP
	sent    []Mail   // mails the host sent, not yet looked at
	events  []string // lines the host wrote, not yet read
	audio   []string // its `audio` and `audio-down` events
	out     []string // the current action's output
	saves   []Record // the records the host saved
	fetched [][]byte // the identities the stack asked the host about
}

func (e *traceEnv) Send(m Mail) error {
	m.Param = append([]byte(nil), m.Param...)
	e.sent = append(e.sent, m)
	return nil
}

func (e *traceEnv) Room() bool { return true }

// Fetch notes the identity the stack asked about (the tests answer it themselves).
func (e *traceEnv) Fetch(ipui []byte) { e.fetched = append(e.fetched, append([]byte(nil), ipui...)) }

// Random is test_stubd.c's random file: byte i is (i * 37 + 11) & 0xFF.
func (e *traceEnv) Random(b []byte) {
	for i := range b {
		b[i] = byte((e.rnd*37 + 11) & 0xFF)
		e.rnd = (e.rnd + 1) % 65536
	}
}

func (e *traceEnv) Log(line string) {}

func (e *traceEnv) Event(to To, line string) {
	if to == ToBridge && !e.bridge {
		return // no bridge client: dect-stubd drops call traffic
	}
	line = strings.TrimSuffix(line, "\n")
	if strings.HasPrefix(line, "audio ") || strings.HasPrefix(line, "audio-down ") {
		// the host's own events (which call the speech belongs to), not
		// dect-stubd's: kept apart, so the golden traces still compare
		e.audio = append(e.audio, line)
		return
	}
	e.events = append(e.events, "ev "+apiRE.ReplaceAllString(line, " api=N"))
}

func (e *traceEnv) Save(_ int, r Record)        { e.saves = append(e.saves, r) }
func (e *traceEnv) Forget(int, string)          {}
func (e *traceEnv) Named(int, string)           {}
func (e *traceEnv) NameVisible(_, _ uint) bool  { return true }
func (e *traceEnv) Switched(int, string)        {}
func (e *traceEnv) Registered(int)              {}
func (e *traceEnv) CapLearned(int, Capability)  {}
func (e *traceEnv) Media(_, _ uint8, _, _ bool) {}

func (e *traceEnv) Clock() (time.Time, bool) {
	return time.Unix(e.wall, 0).UTC(), true
}

func (e *traceEnv) chip(prim uint16, param []byte) {
	e.host.Mail(Mail{Src: 0x02, Dst: 0x14, Prim: prim, Param: param}, e.now)
}

// lc sends one handset Lc frame as 0x0203 segments (07.56 and 08.25 form:
// {handle, 01, seg[5], 00, 00, tag[3], handle}).
func (e *traceEnv) lc(h uint8, frame []byte) {
	for off := 0; off+5 <= len(frame); off += 5 {
		p := make([]byte, 13)
		p[0], p[1], p[12] = h, 0x01, h
		copy(p[2:7], frame[off:off+5])
		e.chip(0x0203, p)
	}
}

func (e *traceEnv) pp(h uint8) *PP {
	if e.pps[h&7] == nil {
		e.pps[h&7] = NewPP()
	}
	return e.pps[h&7]
}

// cplane is the handset reading a base's 0x0244 and acknowledging it.
func (e *traceEnv) cplane(m Mail) {
	h := m.Param[0]
	lsig, _ := e.host.LinkLSIG(h)
	st, msg := e.pp(h).FromBase(lsig, m.Param)
	switch st {
	case PPUnreadable:
		e.out = append(e.out, mailLine(m))
		return
	case PPNone:
		return
	case PPMsg:
		e.out = append(e.out, "fp"+hexLine(msg))
	}
	e.lc(h, e.pp(h).RR())
}

// settle runs the loop at this time until the host sends nothing more.
func (e *traceEnv) settle() {
	for guard := 32; ; guard-- {
		if guard == 0 {
			e.t.Fatal("the host does not settle")
		}
		e.host.Tick(e.now)
		e.out = append(e.out, e.events...)
		e.events = nil
		sent := e.sent
		e.sent = nil
		for _, m := range sent {
			if m.Prim == 0x0244 && len(m.Param) >= 2 {
				e.cplane(m)
			} else {
				e.out = append(e.out, mailLine(m))
			}
		}
		if len(sent) == 0 && len(e.events) == 0 {
			return
		}
	}
}

func hexLine(b []byte) string {
	var s strings.Builder
	for _, v := range b {
		fmt.Fprintf(&s, " %02x", v)
	}
	return s.String()
}

func mailLine(m Mail) string {
	return fmt.Sprintf("mail %04x", m.Prim) + hexLine(m.Param)
}

func parseHex(s string) ([]byte, error) {
	return hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
}

func isOutput(line string) bool {
	return strings.HasPrefix(line, "fp") || strings.HasPrefix(line, "mail ") ||
		strings.HasPrefix(line, "ev ")
}

// subRecord is test_stubd.c's write_sub(): a made-up UAK, the identity as
// both MM and CC portable identity, the traces' PARK as the fixed one.
func subRecord(slot uint, id []byte) Record {
	r := Record{Slot: slot, PortID: id, CCPortID: id,
		CCFixedID: []byte{0x00, 0x01, 0x23, 0x45, 0x60}}
	for k := range r.UAK {
		r.UAK[k] = byte(0xA0 + slot*16 + uint(k))
	}
	return r
}

// applyArgs maps the dect-stubd options the traces use onto Config.
func applyArgs(cfg *Config, args []string) error {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-C":
			cfg.NoCipher = true
		case "-a":
			i++
			if i == len(args) {
				return errors.New("-a without options")
			}
			for _, o := range strings.Split(args[i], ",") {
				k, v, _ := strings.Cut(o, "=")
				n, _ := strconv.ParseUint(v, 0, 32)
				switch k {
				case "twocalls":
					cfg.TwoCalls = true
				case "nolocauth":
					cfg.NoLocAuth = true
				case "bindfirst":
					cfg.BindFirst = true
				case "alertms":
					cfg.AlertMs = uint(n)
				case "dialwait":
					cfg.DialWaitMs = uint(n)
				case "timeupdate":
					cfg.TimeEveryMin = uint(n)
				case "ipuio":
					u := uint32(n)
					cfg.IPUIO = &u
				default:
					return fmt.Errorf("option %q not mapped", k)
				}
			}
		default:
			return fmt.Errorf("argument %q not mapped", args[i])
		}
	}
	return nil
}

type traceStep struct {
	line   int
	action string
	want   []string
}

// readTrace splits a trace into its set-up and its actions with their output.
func readTrace(path string) (setup []string, steps []traceStep, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	started := false
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimRight(line, " \t")
		if line == "" {
			continue
		}
		switch {
		case !started && line == "start":
			started = true
			steps = append(steps, traceStep{line: n, action: "start"})
		case !started:
			setup = append(setup, line)
		case isOutput(line):
			steps[len(steps)-1].want = append(steps[len(steps)-1].want, line)
		default:
			steps = append(steps, traceStep{line: n, action: line})
		}
	}
	return setup, steps, sc.Err()
}

func TestGoldenTraces(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(traceDir, "*.trace"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden traces in %s (submodule checked out?)", traceDir)
	}
	for _, path := range files {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".trace"), func(t *testing.T) {
			replay(t, path, nil)
		})
	}
}

// register.trace with its PIN as the second of a list of registration
// PINs and no base PIN (openDECTcell API 139, adr/0005-registration-flow.md):
// the same air traffic, and the record names the PIN it proved.
func TestRegisterWithPINList(t *testing.T) {
	e := replay(t, filepath.Join(traceDir, "register.trace"), []string{"87654321", "12345678", ""})
	if len(e.saves) == 0 {
		t.Fatal("no record saved")
	}
	if got := e.saves[0].RegPIN1; got != 2 {
		t.Errorf("reg_pin1 %d, want 2 (the second PIN)", got)
	}
}

// replay runs a golden trace. With regPINs the base has no PIN of its own
// and takes those as its registration PINs.
func replay(t *testing.T, path string, regPINs []string) *traceEnv {
	setup, steps, err := readTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimSuffix(filepath.Base(path), ".trace")
	for i := range steps {
		if onePassEarly[name][steps[i].line] && i+1 < len(steps) && len(steps[i+1].want) > 0 {
			steps[i].want = append(steps[i].want, steps[i+1].want[0])
			steps[i+1].want = steps[i+1].want[1:]
		}
	}
	cfg := Config{PIN: "12345678", UAKTable: 0x9518, CheckDelayMs: 50, Stub: "dectfw_secondlevel_441.hex", NoDummyKey: true}
	if replayTune != nil {
		replayTune(&cfg)
	}
	type rec struct {
		free bool
		r    Record
	}
	var recs []rec
	for _, l := range setup {
		verb, arg, _ := strings.Cut(l, " ")
		switch verb {
		case "sub":
			s, id, _ := strings.Cut(arg, " ")
			slot, _ := strconv.ParseUint(s, 10, 8)
			b, err := parseHex(id)
			if err != nil {
				t.Fatalf("%s: %v", l, err)
			}
			recs = append(recs, rec{r: subRecord(uint(slot), b)})
		case "store":
			recs = append(recs, rec{free: true})
		case "args":
			if err := applyArgs(&cfg, strings.Fields(arg)); err != nil {
				t.Fatalf("%s: %v", l, err)
			}
		default:
			t.Fatalf("before start: %q", l)
		}
	}

	e := &traceEnv{t: t, now: 1000, wall: 1793350800, pps: map[uint8]*PP{}}
	defer func() {
		for _, p := range e.pps {
			p.Close()
		}
	}()
	if regPINs != nil {
		cfg.PIN = ""
	}
	e.host = NewHost(e, cfg)
	defer e.host.Close()
	if regPINs != nil {
		e.host.SetRegPINs(regPINs)
	}
	for i, r := range recs {
		if r.free {
			e.host.AddFreeStore(i, [3]byte{})
		} else {
			e.host.AddRecord(i, r.r)
		}
	}
	if err := e.host.Start(); err != nil {
		t.Fatal(err)
	}

	cur := uint8(1)
	for _, st := range steps {
		verb, arg, _ := strings.Cut(st.action, " ")
		switch verb {
		case "start":
			// the chip's contact answer carries the traces' made-up RFPI
			e.chip(0xF601, []byte{0x00, 0x01, 0x23, 0x45, 0x60})
			e.host.RadioUp(true)
			e.host.Hello()
			e.settle()
		case "up":
			h, _ := strconv.ParseUint(arg, 10, 8)
			cur = uint8(h)
			if p := e.pps[cur&7]; p != nil {
				p.Close()
			}
			e.pps[cur&7] = NewPP()
			e.chip(0x0200, []byte{cur})
			e.settle()
		case "on":
			h, _ := strconv.ParseUint(arg, 10, 8)
			cur = uint8(h)
			continue
		case "down":
			e.chip(0x0201, []byte{cur})
			e.settle()
		case "chip":
			p, rest, _ := strings.Cut(arg, " ")
			prim, _ := strconv.ParseUint(p, 16, 16)
			b, err := parseHex(rest)
			if err != nil {
				t.Fatalf("line %d: %v", st.line, err)
			}
			e.chip(uint16(prim), b)
			e.settle()
		case "pp":
			b, err := parseHex(arg)
			if err != nil || len(b) == 0 {
				t.Fatalf("line %d: bad NWK message", st.line)
			}
			fr, err := e.pp(cur).Send(b)
			if err != nil {
				t.Fatalf("line %d: %v", st.line, err)
			}
			e.lc(cur, fr)
			e.settle()
		case "tick":
			ms, _ := strconv.Atoi(arg)
			for d := 0; d < ms; d += 10 {
				e.now += 10
				e.settle()
			}
			e.wall += int64(ms / 1000)
		case "ctl":
			if e.host.Ctl(arg, e.now, e.bridge) {
				e.bridge = true
			}
			e.settle()
		default:
			t.Fatalf("line %d: unknown action %q", st.line, st.action)
		}
		compare(t, path, st, e.out)
		e.out = nil
	}
	if replayHook != nil {
		replayHook(e)
	}
	return e
}

// replayTune, when set, changes the host's configuration before a replay.
var replayTune func(*Config)

// replayHook, when set, runs at the end of replay, with the host still open.
var replayHook func(e *traceEnv)

// gotFilter, when set, rewrites each produced line before it is compared (a test
// that changes the configuration maps the changed lines back to the golden ones
// and checks them on the way).
var gotFilter func(string) string

func compare(t *testing.T, path string, st traceStep, got []string) {
	t.Helper()
	if gotFilter != nil {
		got = append([]string(nil), got...)
		for i := range got {
			got[i] = gotFilter(got[i])
		}
	}
	var want []string
	for _, w := range st.want {
		if !skipLine(w, st.action == "start") {
			want = append(want, w)
		}
	}
	for i := 0; i < len(want) || i < len(got); i++ {
		g, w := "(nothing)", "(nothing)"
		if i < len(got) {
			g = got[i]
		}
		if i < len(want) {
			w = want[i]
		}
		if g != w {
			t.Fatalf("%s:%d (%s): output %d\n  want %s\n  got  %s", filepath.Base(path),
				st.line, st.action, i+1, w, g)
		}
	}
}

// A record that holds a handset in a chip slot becomes a free store again, in place: the
// slot is cleared in the chip, the TPUI is the new one, and a second recycle finds a free store.
func TestRecycleAfterEnrolment(t *testing.T) {
	replayHook = func(e *traceEnv) {
		if len(e.saves) == 0 {
			t.Fatal("no record saved")
		}
		slot := e.saves[0].Slot
		if slot < 1 || slot > 6 {
			t.Fatalf("enrolled in slot %d, not a chip slot", slot)
		}
		e.sent = nil
		tp := [3]byte{0, 0x11, 0x2A}
		var got Recycled
		done := -1
		for i := 0; i < 8; i++ { // the record is one of the first few
			if got = e.host.Recycle(i, tp); got == RecycleDone {
				done = i
				break
			}
			if got == RecycleBusy {
				t.Fatalf("record %d: not quiet after the trace", i)
			}
		}
		if got != RecycleDone {
			t.Fatal("no record could be recycled")
		}
		var clear, zeros bool
		for _, m := range e.sent {
			switch {
			case m.Prim == 0x0247 && len(m.Param) == 3 && uint(m.Param[0]) == slot && m.Param[1] == 0:
				clear = true
			case m.Prim == 0x020A && len(m.Param) > 1 && uint(m.Param[0]) == slot && allZero(m.Param[1:]):
				zeros = true
			}
		}
		if !clear || !zeros {
			t.Errorf("chip slot %d: clear %v, key zeros %v", slot, clear, zeros)
		}
		if again := e.host.Recycle(done, tp); again != RecycleNo {
			t.Errorf("a free store: Recycle answered %v, want RecycleNo", again)
		}
	}
	defer func() { replayHook = nil }()
	replay(t, filepath.Join(traceDir, "register.trace"), nil)
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// The host tells the bridge which call the speech belongs to: `audio` with the
// call's number when the lane is bound, `audio-down` with the link when it is
// released (the controller's media session follows them).
func TestAudioEventsNameTheCall(t *testing.T) {
	e := replay(t, filepath.Join(traceDir, "call-out.trace"), nil)
	if len(e.audio) < 2 {
		t.Fatalf("audio events: %q", e.audio)
	}
	if want := "audio call=1 handle=1 channel=1 wideband=0"; e.audio[0] != want {
		t.Errorf("first event %q, want %q", e.audio[0], want)
	}
	if last := e.audio[len(e.audio)-1]; !strings.HasPrefix(last, "audio-down handle=1") {
		t.Errorf("last event %q, want audio-down for handle 1", last)
	}
}

// The audio bind carries the mask and the narrowband bind byte the daemon's init
// script gives a box (mask 0x0002, codec 3), which the cell needs to find the
// call's lane: the chip echoes the mask in its long 0x0242, and a mask of 0
// names no lane ("its bind's mask names none", first call on air, 2026-11-01).
// Everything else of the golden trace stays as it was.
func TestAudioBindCarriesTheConfiguredMask(t *testing.T) {
	binds := 0
	replayTune = func(c *Config) { c.AudioMask, c.AudioCodec = 0x0002, 3 }
	gotFilter = func(l string) string {
		if l == "mail 0230 01 01 02 00 00 03" {
			binds++
			return "mail 0230 01 01 00 00 00 00"
		}
		return l
	}
	defer func() { replayTune, gotFilter = nil, nil }()
	replay(t, filepath.Join(traceDir, "call-out.trace"), nil)
	if binds != 1 {
		t.Errorf("%d binds with mask 0x0002 and codec 3, want 1", binds)
	}
}
