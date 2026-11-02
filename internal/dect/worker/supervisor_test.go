// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// The test binary is its own worker: the supervisor starts it with this variable set.
func TestMain(m *testing.M) {
	if os.Getenv("OPENDECTCELL_WORKER_TEST") == "1" {
		Harden()
		f := os.NewFile(3, "ctl")
		conn, err := net.FileConn(f)
		if err != nil {
			os.Exit(3)
		}
		if err := Confine(); err != nil {
			os.Exit(4)
		}
		_ = Serve(conn, func(e stack.Env, cfg stack.Config) stack.Stack {
			return &fakeStack{env: e, records: map[int]stack.Record{}}
		})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func newSupervisor(t *testing.T, n int) *Supervisor {
	t.Helper()
	s := &Supervisor{Exe: os.Args[0], Args: []string{}, Env: []string{"OPENDECTCELL_WORKER_TEST=1"}, N: n,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		PingEvery: 100 * time.Millisecond, PingTimeout: 150 * time.Millisecond, BackoffMin: 50 * time.Millisecond, BackoffMax: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	t.Cleanup(func() { cancel(); s.Wait() })
	waitReady(t, s, n)
	return s
}

func waitReady(t *testing.T, s *Supervisor, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		up := 0
		for _, w := range s.workers {
			if w.rem != nil && w.rem.Alive() {
				up++
			}
		}
		s.mu.Unlock()
		if up >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d workers up", up, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newStackIn(t *testing.T, s *Supervisor) (stack.Stack, *ownerEnv) {
	t.Helper()
	env := &ownerEnv{}
	st := s.NewStack(env, stack.Config{}, func() uint64 { return 1000 })
	if st == nil {
		t.Fatal("no stack")
	}
	return st, env
}

// Stacks go to the worker with the fewest, so a crash takes down only the cells that were in it:
// the other worker's stack runs on, and the crashed worker comes back and takes stacks again.
func TestSupervisorContainsACrash(t *testing.T) {
	s := newSupervisor(t, 2)
	a, _ := newStackIn(t, s)
	b, _ := newStackIn(t, s)
	defer b.Close()
	if !a.Ctl("bridge", 1, false) || !b.Ctl("bridge", 1, false) {
		t.Fatal("the stacks do not answer")
	}
	a.Ctl("crash", 2, false) // the worker of a exits
	deadline := time.Now().Add(5 * time.Second)
	for a.(stack.Pumper).Alive() {
		if time.Now().After(deadline) {
			t.Fatal("the stack of the crashed worker is alive")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !b.(stack.Pumper).Alive() || !b.Ctl("bridge", 3, false) {
		t.Error("the other worker's stack was hurt by the crash")
	}
	// the crashed worker is started again; the supervisor has two workers up once more
	waitReady(t, s, 2)
	c, _ := newStackIn(t, s)
	defer c.Close()
	if !c.Ctl("bridge", 4, false) {
		t.Error("a stack in the restarted worker does not answer")
	}
	a.Close() // a dead stack's Close does not hang
}

// A worker that stops answering pings (a hang in the C code) is killed and restarted.
func TestSupervisorKillsAHungWorker(t *testing.T) {
	s := newSupervisor(t, 1)
	a, _ := newStackIn(t, s)
	old := callTimeout
	callTimeout = 10 * time.Second // the ping, not a call, must find it
	defer func() { callTimeout = old }()
	done := make(chan struct{})
	go func() { a.Ctl("stop", 1, false); close(done) }() // the process stops itself
	deadline := time.Now().Add(5 * time.Second)
	for a.(stack.Pumper).Alive() {
		if time.Now().After(deadline) {
			t.Fatal("a hung worker was not noticed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	<-done // the call that was waiting returned when the worker was declared dead
	waitReady(t, s, 1)
	c, _ := newStackIn(t, s)
	defer c.Close()
	if !c.Ctl("bridge", 2, false) {
		t.Error("the restarted worker does not answer")
	}
}

// With no worker up there is no stack, and the caller can ask again.
func TestSupervisorWithoutWorkers(t *testing.T) {
	s := &Supervisor{Exe: "/nonexistent", N: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), BackoffMin: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	defer func() { cancel(); s.Wait() }()
	time.Sleep(50 * time.Millisecond)
	if s.Ready() || s.NewStack(&ownerEnv{}, stack.Config{}, func() uint64 { return 1 }) != nil {
		t.Error("a stack without a worker")
	}
}

// A confined worker cannot open files, make connections or start programs, on any thread; its socket
// to the controller keeps working. The test binary is the worker (TestMain).
func TestConfinedWorkerCannotReachFilesOrNetwork(t *testing.T) {
	if os.Getenv("OPENDECTCELL_CONFINE_PROBE") == "1" {
		t.Skip("child")
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestConfineProbe", "-test.v")
	cmd.Env = []string{"OPENDECTCELL_CONFINE_PROBE=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, out)
	}
}

// TestConfineProbe runs only as the child of the test above: it confines itself and tries the denied calls.
func TestConfineProbe(t *testing.T) {
	if os.Getenv("OPENDECTCELL_CONFINE_PROBE") != "1" {
		t.Skip("run by TestConfinedWorkerCannotReachFilesOrNetwork")
	}
	keep, err := os.OpenFile("/dev/null", os.O_WRONLY, 0) // opened before, still usable after
	if err != nil {
		t.Fatal(err)
	}
	if err := Confine(); err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if _, err := os.Open("/etc/passwd"); err == nil {
		t.Error("a confined process opened a file")
	}
	if f, err := os.OpenFile("/tmp/opendectcell-confine-probe", os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
		os.Remove("/tmp/opendectcell-confine-probe")
		t.Error("a confined process created a file")
	}
	if c, err := net.Dial("tcp", "127.0.0.1:9"); err == nil {
		c.Close()
		t.Error("a confined process made a connection")
	}
	if err := exec.Command("/bin/true").Run(); err == nil {
		t.Error("a confined process started a program")
	}
	// every thread is confined, not just the one that called it
	done := make(chan error, 8)
	for range 8 {
		go func() {
			runtime.LockOSThread()
			_, err := os.Open("/etc/hostname")
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err == nil {
			t.Error("a thread other than the confining one opened a file")
		}
	}
	if _, err := keep.Write([]byte("x")); err != nil {
		t.Errorf("an already open file stopped working: %v", err)
	}
}
