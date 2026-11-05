// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package media

// A load test for the media engine: N calls at once, each with a SIP leg (SDES-SRTP) and a cell leg (SRTP), the
// pipeline between them in this process, and the two far ends (the PBX and the cell) in a child process, so that
// this process's CPU time is the engine's and not the traffic generator's. Real sockets on loopback, real time.
//
// It does nothing unless OPENDECTCELL_LOAD_CALLS is set. Settings:
//
//	OPENDECTCELL_LOAD_CALLS    number of calls
//	OPENDECTCELL_LOAD_KINDS    comma list of g722, pcma-wide, pcma-narrow; the calls take them in turn (default g722)
//	OPENDECTCELL_LOAD_SECONDS  measured time after 5 s of warm-up (default 20)
//
//	go test -c -o media.test ./internal/media && OPENDECTCELL_LOAD_CALLS=40 ./media.test -test.run '^TestLoadEngine$' -test.v
//
// It prints one line "RESULT {json}" per run.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/media/codec"
	"github.com/openDECTcell/opendectcell-controller/internal/media/quality"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

const (
	loadPeersEnv  = "OPENDECTCELL_LOAD_PEERS"
	loadSIPBase   = 20000 // the engine's SIP media ports (below the ephemeral range, which the cell sockets draw from)
	loadPBXBase   = 24000 // the PBX's, two per call
	loadWarm      = 5 * time.Second
	loadSuite     = "AES_CM_128_HMAC_SHA1_80"
	loadCellPT    = 96
	loadSendShard = 4
)

type loadCall struct {
	Kind    string
	PBXPort int
	CtlPort int // the engine's SIP media port
	K1, K2  []byte
	MO      wire.MediaOpenMsg
}

type loadConfig struct {
	Calls   []loadCall
	Start   time.Time
	Seconds float64
}

// loadPlan is what the controller would choose for the kind, and the payload type negotiated with the PBX.
func loadPlan(t *testing.T, kind string) (Plan, uint8, string) {
	t.Helper()
	var (
		plan Plan
		err  error
		pt   uint8
		name string
	)
	switch kind {
	case "g722":
		plan, err = Choose(wideCell, true, codec.G722)
		pt, name = 9, "G722"
	case "pcma-wide":
		plan, err = Choose(wideCell, true, codec.PCMA)
		pt, name = 8, "PCMA"
	case "pcma-narrow":
		plan, err = Choose(narrowCell, false, codec.PCMA)
		pt, name = 8, "PCMA"
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	return plan, pt, name
}

// loadFrames is two seconds of speech-like audio, encoded once, in 20 ms frames.
func loadFrames(c *codec.Codec, seed uint64) ([][]byte, uint32) {
	src := quality.SpeechLike(-20, c.SampleRate, c.SampleRate*2, seed)
	n := c.SampleRate / 50
	enc := c.NewEncoder()
	var out [][]byte
	for i := 0; i+n <= len(src); i += n {
		out = append(out, enc.Encode(nil, src[i:i+n]))
	}
	return out, c.TimestampUnits(n)
}

type loadEvents struct{}

func (loadEvents) SendDTMF(context.Context, rune, time.Duration) error { return nil }

func rusage() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// cpuStat is the machine's busy and total jiffies from /proc/stat.
func cpuStat() (busy, total float64) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	f := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
	for i, s := range f[1:] {
		v, _ := strconv.ParseFloat(s, 64)
		total += v
		if i != 3 && i != 4 { // idle, iowait
			busy += v
		}
	}
	return busy, total
}

func rssMiB() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			kb, _ := strconv.ParseFloat(strings.Fields(l)[1], 64)
			return kb / 1024
		}
	}
	return 0
}

func TestLoadEngine(t *testing.T) {
	v := os.Getenv("OPENDECTCELL_LOAD_CALLS")
	if v == "" {
		t.Skip("OPENDECTCELL_LOAD_CALLS not set")
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("OPENDECTCELL_LOAD_CALLS=%q", v)
	}
	kinds := strings.Split(os.Getenv("OPENDECTCELL_LOAD_KINDS"), ",")
	if kinds[0] == "" {
		kinds = []string{"g722"}
	}
	secs := 20.0
	if s := os.Getenv("OPENDECTCELL_LOAD_SECONDS"); s != "" {
		if secs, err = strconv.ParseFloat(s, 64); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ports := &rtp.Ports{IP: lo, Min: loadSIPBase, Max: loadSIPBase + 2*n + 1}
	cfg := loadConfig{Seconds: secs}
	type engineCall struct {
		p    *Pipeline
		ctl  *rtp.Endpoint
		cell *CellLeg
	}
	var calls []engineCall
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		kind := kinds[i%len(kinds)]
		plan, pt, name := loadPlan(t, kind)
		ctl, err := ports.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		k1, _ := sdp.NewKey(loadSuite)
		k2, _ := sdp.NewKey(loadSuite)
		pbxPort := loadPBXBase + 2*i
		if err := ctl.Configure(&sdp.Media{Remote: netip.AddrPortFrom(lo, uint16(pbxPort)), Codec: sdp.Codec{Name: name, PT: pt, Clock: 8000},
			LocalKey: &sdp.Crypto{Suite: loadSuite, Key: k1}, RemoteKey: &sdp.Crypto{Suite: loadSuite, Key: k2}}); err != nil {
			t.Fatal(err)
		}
		conn := udp(t)
		mo, err := NewMediaOpen(uint32(i+1), 1, plan, loadCellPT, port(conn), wire.SRTPAES128SHA80)
		if err != nil {
			t.Fatal(err)
		}
		cellLeg, err := NewCellLeg(conn, mo, 8000)
		if err != nil {
			t.Fatal(err)
		}
		pc := Config{Plan: plan, SIP: SIPLeg(ctl), SIPPayloadType: pt, Cell: cellLeg, CellPayload: loadCellPT}
		if kind == "g722" {
			pc.Events = loadEvents{} // RFC 4733 negotiated: the uplink may pass through
		}
		p, err := New(pc)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, engineCall{p, ctl, cellLeg})
		cfg.Calls = append(cfg.Calls, loadCall{Kind: kind, PBXPort: pbxPort, CtlPort: ctl.LocalPort(), K1: k1, K2: k2, MO: mo})
		wg.Add(1)
		go func() { defer wg.Done(); _ = p.Run(ctx) }()
	}

	cfg.Start = time.Now().Add(3 * time.Second)
	dir := t.TempDir()
	cfgPath := dir + "/load.json"
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run", "^TestLoadPeers$", "-test.v")
	child.Env = append(os.Environ(), loadPeersEnv+"="+cfgPath)
	child.Stdout, child.Stderr = os.Stderr, os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Until(cfg.Start.Add(loadWarm)))
	cpu0, w0 := rusage(), time.Now()
	b0, t0 := cpuStat()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	time.Sleep(time.Duration(secs * float64(time.Second)))
	cpu1, w1 := rusage(), time.Now()
	b1, t1 := cpuStat()
	rss := rssMiB()
	var down, up DirStats
	for _, c := range calls {
		st := c.p.Stats(time.Now())
		down.Sent += st.Down.Sent
		down.Concealed += st.Down.Concealed
		down.Silence += st.Down.Silence
		down.SendErrs += st.Down.SendErrs
		up.Sent += st.Up.Sent
		up.Concealed += st.Up.Concealed
		up.Silence += st.Up.Silence
		up.SendErrs += st.Up.SendErrs
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("peers: %v", err)
	}
	cancel()
	wg.Wait()

	var peers loadPeersResult
	rb, err := os.ReadFile(cfgPath + ".result")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rb, &peers); err != nil {
		t.Fatal(err)
	}
	wall := w1.Sub(w0).Seconds()
	cores := (cpu1 - cpu0).Seconds() / wall
	res := map[string]any{
		"calls": n, "kinds": strings.Join(kinds, ","), "seconds": wall,
		"engine_cores": loadRound(cores, 3), "engine_cores_per_call": loadRound(cores/float64(n), 4),
		"machine_busy_pct": loadRound(100*(b1-b0)/(t1-t0), 1), "engine_rss_mib": loadRound(rss, 1),
		"down_sent": down.Sent, "down_concealed": down.Concealed, "down_silence": down.Silence, "down_senderrs": down.SendErrs,
		"up_sent": up.Sent, "up_concealed": up.Concealed, "up_silence": up.Silence, "up_senderrs": up.SendErrs,
		"peers": peers,
	}
	out, _ := json.Marshal(res)
	fmt.Printf("RESULT %s\n", out)
}

func loadRound(v float64, d int) float64 {
	p := 1.0
	for i := 0; i < d; i++ {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}

// one received flow
type loadRx struct {
	from          time.Time // packets before this (the warm-up) are not counted
	n             int
	first         time.Time
	ts0           uint32
	seq0, seqLast uint16
	wrap          int
	delays        []float64 // ms, arrival minus the RTP clock
	maxGap        time.Duration
	last          time.Time
}

func (r *loadRx) add(p Packet, at time.Time) {
	if at.Before(r.from) {
		return
	}
	if r.n == 0 {
		r.first, r.ts0, r.seq0 = at, p.TS, p.Seq
	} else {
		if p.Seq < r.seqLast && r.seqLast-p.Seq > 30000 {
			r.wrap++
		}
		if g := at.Sub(r.last); g > r.maxGap {
			r.maxGap = g
		}
	}
	r.n++
	r.seqLast, r.last = p.Seq, at
	r.delays = append(r.delays, float64(at.Sub(r.first))/1e6-float64(p.TS-r.ts0)/8)
}

func (r *loadRx) expected() int { return r.wrap*65536 + int(r.seqLast) - int(r.seq0) + 1 }

type loadFlow struct {
	Received, Lost     int
	MaxGapMS           float64
	DelayP50, DelayP99 float64 // variation over the flow's minimum, ms
	DelayMax           float64
}

type loadPeersResult struct {
	Calls                          int
	GeneratorLateTicks, TotalTicks int
	ToCell, ToSIP                  []loadFlow // per call: what the cell and the PBX received
	Summary                        map[string]float64
}

func summarise(name string, flows []loadFlow, out map[string]float64) {
	var rx, lost int
	var worstLoss, gap, p99, peak float64
	for _, f := range flows {
		rx += f.Received
		lost += f.Lost
		if f.Received+f.Lost > 0 {
			if l := float64(f.Lost) / float64(f.Received+f.Lost); l > worstLoss {
				worstLoss = l
			}
		}
		if f.MaxGapMS > gap {
			gap = f.MaxGapMS
		}
		if f.DelayP99 > p99 {
			p99 = f.DelayP99
		}
		if f.DelayMax > peak {
			peak = f.DelayMax
		}
	}
	out[name+"_received"] = float64(rx)
	out[name+"_lost_pct"] = loadRound(100*float64(lost)/float64(max2(rx+lost, 1)), 3)
	out[name+"_worst_call_lost_pct"] = loadRound(100*worstLoss, 2)
	out[name+"_max_gap_ms"] = loadRound(gap, 1)
	out[name+"_worst_p99_delay_var_ms"] = loadRound(p99, 1)
	out[name+"_max_delay_var_ms"] = loadRound(peak, 1)
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (r *loadRx) flow() loadFlow {
	f := loadFlow{Received: r.n, MaxGapMS: float64(r.maxGap) / 1e6}
	if r.n == 0 {
		return f
	}
	f.Lost = r.expected() - r.n
	if f.Lost < 0 {
		f.Lost = 0
	}
	d := append([]float64(nil), r.delays...)
	sort.Float64s(d)
	base := d[0]
	f.DelayP50, f.DelayP99, f.DelayMax = d[len(d)/2]-base, d[len(d)*99/100]-base, d[len(d)-1]-base
	return f
}

// TestLoadPeers is the far ends: it only runs as the engine's child.
func TestLoadPeers(t *testing.T) {
	path := os.Getenv(loadPeersEnv)
	if path == "" {
		t.Skip("child of TestLoadEngine only")
	}
	var cfg loadConfig
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}

	type peer struct {
		sip, cell  Leg
		sipFrames  [][]byte
		cellFrames [][]byte
		sipUnits   uint32
		cellUnits  uint32
		sipPT      uint8
		rxSIP      loadRx // what the PBX hears
		rxCell     loadRx // what the cell hears
		sipCodec   *codec.Codec
		cellCodec  *codec.Codec
	}
	ctx := context.Background()
	peers := make([]*peer, len(cfg.Calls))
	for i, c := range cfg.Calls {
		plan, pt, name := loadPlan(t, c.Kind)
		pp := &rtp.Ports{IP: lo, Min: c.PBXPort, Max: c.PBXPort + 1}
		e, err := pp.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Configure(&sdp.Media{Remote: netip.AddrPortFrom(lo, uint16(c.CtlPort)), Codec: sdp.Codec{Name: name, PT: pt, Clock: 8000},
			LocalKey: &sdp.Crypto{Suite: loadSuite, Key: c.K2}, RemoteKey: &sdp.Crypto{Suite: loadSuite, Key: c.K1}}); err != nil {
			t.Fatal(err)
		}
		conn, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(lo, 0)))
		if err != nil {
			t.Fatal(err)
		}
		mirror := c.MO
		mirror.SSRCUp, mirror.SSRCDown, mirror.KeyUp, mirror.KeyDown = c.MO.SSRCDown, c.MO.SSRCUp, c.MO.KeyDown, c.MO.KeyUp
		cl, err := NewCellLeg(conn, mirror, 8000)
		if err != nil {
			t.Fatal(err)
		}
		cl.remote = c.MO.CtlAddr
		p := &peer{sip: SIPLeg(e), cell: cl, sipPT: pt, sipCodec: plan.SIP, cellCodec: FormatCodec(plan.CellUp)}
		p.sipFrames, p.sipUnits = loadFrames(plan.SIP, uint64(i)+1)
		p.cellFrames, p.cellUnits = loadFrames(p.cellCodec, uint64(i)+1001)
		p.rxSIP.from, p.rxCell.from = cfg.Start.Add(loadWarm), cfg.Start.Add(loadWarm)
		peers[i] = p
	}

	until := cfg.Start.Add(loadWarm + time.Duration(cfg.Seconds*float64(time.Second)) + time.Second)
	var rw sync.WaitGroup
	var mu sync.Mutex
	for _, p := range peers {
		rw.Add(2)
		go func() {
			defer rw.Done()
			for {
				pk, err := p.sip.ReadPacket(until.Add(time.Second))
				if err != nil {
					return
				}
				mu.Lock()
				p.rxSIP.add(pk, time.Now())
				mu.Unlock()
			}
		}()
		go func() {
			defer rw.Done()
			for {
				pk, err := p.cell.ReadPacket(until.Add(time.Second))
				if err != nil {
					return
				}
				mu.Lock()
				p.rxCell.add(pk, time.Now())
				mu.Unlock()
			}
		}()
	}

	var late, ticks int
	var sw sync.WaitGroup
	for s := 0; s < loadSendShard; s++ {
		sw.Add(1)
		go func() {
			defer sw.Done()
			time.Sleep(time.Until(cfg.Start) + time.Duration(s)*5*time.Millisecond)
			tk := time.NewTicker(20 * time.Millisecond)
			defer tk.Stop()
			for f := 0; ; f++ {
				at := <-tk.C
				if at.After(until) {
					return
				}
				lag := time.Since(at)
				mu.Lock()
				ticks++
				if lag > 5*time.Millisecond {
					late++
				}
				mu.Unlock()
				for i := s; i < len(peers); i += loadSendShard {
					p := peers[i]
					_ = p.sip.WritePacket(p.sipPT, p.sipFrames[f%len(p.sipFrames)], p.sipUnits, f == 0)
					_ = p.cell.WritePacket(loadCellPT, p.cellFrames[f%len(p.cellFrames)], p.cellUnits, f == 0)
				}
			}
		}()
	}
	sw.Wait()
	rw.Wait()

	if d := os.Getenv("OPENDECTCELL_LOAD_DUMP"); d != "" { // the timeline of call 0, to look at
		var sb strings.Builder
		for i, v := range peers[0].rxSIP.delays {
			fmt.Fprintf(&sb, "sip %d %.1f\n", i, v)
		}
		for i, v := range peers[0].rxCell.delays {
			fmt.Fprintf(&sb, "cell %d %.1f\n", i, v)
		}
		_ = os.WriteFile(d, []byte(sb.String()), 0o600)
	}
	res := loadPeersResult{Calls: len(peers), GeneratorLateTicks: late, TotalTicks: ticks, Summary: map[string]float64{}}
	for _, p := range peers {
		res.ToCell = append(res.ToCell, p.rxCell.flow())
		res.ToSIP = append(res.ToSIP, p.rxSIP.flow())
	}
	summarise("to_cell", res.ToCell, res.Summary)
	summarise("to_sip", res.ToSIP, res.Summary)
	res.ToCell, res.ToSIP = nil, nil // the summary is enough
	out, _ := json.Marshal(res)
	if err := os.WriteFile(path+".result", out, 0o600); err != nil {
		t.Fatal(err)
	}
}
