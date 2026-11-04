// SPDX-License-Identifier: AGPL-3.0-or-later

package cell_test

import (
	"context"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/celltest"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
)

// Many cells on one server: all of them connect (from one address, so the pre-authentication limit per address is in
// play), all stay connected while they send heartbeats and STATS, and every cell gets its last-seen time. The size is OPENDECTCELL_SCALE_CELLS (default 40; the goal is hundreds of cells, so try 300).
func TestManyCellsStayConnected(t *testing.T) {
	n := 40
	if v := os.Getenv("OPENDECTCELL_SCALE_CELLS"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 {
			t.Fatalf("OPENDECTCELL_SCALE_CELLS=%q", v)
		}
	}
	if testing.Short() {
		n = 10
	}
	e := newEnvTuned(t, func(l net.Listener) net.Listener { return l }, func(s *cell.Server) {
		s.Heartbeat, s.Dead = 200*time.Millisecond, 3*time.Second // far from a real cell's 5 s, but long enough for a loaded test machine
	})
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Minute)
	defer cancel()

	// enrolment: a token each (an administrator may hold 50 open ones)
	cells := make([]*celltest.Cell, n)
	enrolStart := time.Now()
	for i := range cells {
		_, es, err := e.token(t, e.admin, "scale", 0, true)
		if err != nil {
			t.Fatal(err)
		}
		c, status, err := celltest.Enroll(ctx, es, "scale")
		if err != nil || status != wire.EnrollOK {
			t.Fatalf("enrol %d: %d %v", i, status, err)
		}
		cells[i] = c
	}
	enrolled := time.Since(enrolStart)

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	goroutinesBefore := runtime.NumGoroutine()

	// every cell connects at once; the server turns away what its per-address limit does not allow and the cell tries again,
	// as a real cell does after a power cut that takes a whole site down at the same moment
	var (
		wg        sync.WaitGroup
		turned    atomic.Int64
		heartbeat atomic.Int64
		dropped   atomic.Int64
		stop      = make(chan struct{})
		connected = make(chan struct{}, n)
	)
	connectStart := time.Now()
	for _, c := range cells {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var cn *celltest.Conn
			for {
				var err error
				if cn, err = c.Connect(ctx, celltest.Options{}); err == nil {
					break
				}
				turned.Add(1)
				select {
				case <-time.After(50 * time.Millisecond):
				case <-stop:
					return
				case <-ctx.Done():
					return
				}
			}
			defer cn.Close()
			connected <- struct{}{}
			go func() { // the cell reads what the controller sends, so nothing backs up
				for {
					f, err := cn.Next(time.Second)
					if err == nil && f.Type == wire.HeartbeatEcho {
						heartbeat.Add(1)
					}
					select {
					case <-stop:
						return
					default:
					}
				}
			}()
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for i := uint32(1); ; i++ {
				select {
				case <-stop:
					return
				case <-tick.C:
				}
				if err := cn.Send(wire.Frame{Type: wire.Heartbeat, Body: wire.HeartbeatBody(i)}); err != nil {
					dropped.Add(1)
					return
				}
				if i%2 == 0 {
					if err := cn.Send(wire.Frame{Type: wire.Stats, Body: wire.EncodeStats(map[uint16]uint64{1: uint64(i)})}); err != nil {
						dropped.Add(1)
						return
					}
				}
			}
		}()
	}
	for range cells {
		select {
		case <-connected:
		case <-ctx.Done():
			t.Fatalf("only %d of %d cells connected", len(connected), n)
		}
	}
	connectedIn := time.Since(connectStart)

	online := func() int {
		k := 0
		for _, c := range cells {
			if e.srv.CellStatus(c.ID).Online {
				k++
			}
		}
		return k
	}
	deadline := time.Now().Add(10 * time.Second)
	for online() < n && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if k := online(); k != n {
		t.Fatalf("%d of %d cells online after they connected", k, n)
	}

	// let them run: heartbeats and STATS flow
	const run = 3 * time.Second
	time.Sleep(run)
	if k := online(); k != n {
		t.Errorf("%d of %d cells online after %v of traffic", k, n, run)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	goroutines := runtime.NumGoroutine() - goroutinesBefore
	close(stop)
	wg.Wait()

	if d := dropped.Load(); d != 0 {
		t.Errorf("%d cells lost their link", d)
	}
	if heartbeat.Load() < int64(n) {
		t.Errorf("only %d heartbeat echoes for %d cells", heartbeat.Load(), n)
	}
	var stamped int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM cell WHERE last_seen_at IS NOT NULL").Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != n {
		t.Errorf("%d of %d cells have a last-seen time", stamped, n)
	}
	t.Logf("%d cells: enrolled in %v (%.0f ms each), all connected in %v (%d connections turned away and retried), "+
		"goroutines +%d (%.1f per cell), heap +%.1f MiB (%.0f KiB per cell), %d heartbeat echoes in %v",
		n, enrolled.Round(time.Millisecond), float64(enrolled.Milliseconds())/float64(n), connectedIn.Round(time.Millisecond), turned.Load(),
		goroutines, float64(goroutines)/float64(n), float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20),
		float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/1024/float64(n), heartbeat.Load(), run)
}
