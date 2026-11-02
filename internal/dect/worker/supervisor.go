// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// Supervisor runs N worker processes (the controller's own binary, started as
// `opendectcell-controller cellworker`), restarts one that ends or stops answering,
// and makes stacks in the one with the fewest.
type Supervisor struct {
	// Exe and Args start a worker; the socket is its file 3. Exe "": this binary,
	// Args nil: cellworker.
	Exe  string
	Args []string
	// Env is the worker's whole environment: none by default, so the master
	// key never reaches it.
	Env []string
	// N is how many workers run; 0: one per CPU.
	N   int
	Log *slog.Logger

	// PingEvery and PingTimeout are the watchdog (defaults 2 s and 2 s); a worker
	// that misses a ping is killed.
	PingEvery, PingTimeout time.Duration
	// Backoff bounds the wait before a worker that ended is started again
	// (defaults 1 s to 60 s); it starts over after StableAfter (5 min).
	BackoffMin, BackoffMax, StableAfter time.Duration

	mu      sync.Mutex
	workers []*slot
	wg      sync.WaitGroup
}

// slot is one worker process and what the supervisor knows of it.
type slot struct {
	n   int
	rem *Remote // nil while the worker is down
}

func (s *Supervisor) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func def(v, d time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return d
}

// Start starts the workers and watches them until ctx ends, then stops them.
func (s *Supervisor) Start(ctx context.Context) {
	n := s.N
	if n <= 0 {
		n = runtime.NumCPU()
	}
	s.mu.Lock()
	for i := range n {
		s.workers = append(s.workers, &slot{n: i})
	}
	s.mu.Unlock()
	for _, w := range s.workers {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.watch(ctx, w) }()
	}
}

// Wait waits for the watchers to end after ctx did.
func (s *Supervisor) Wait() { s.wg.Wait() }

// Ready says whether at least one worker is up.
func (s *Supervisor) Ready() bool { return s.pick() != nil }

// NewStack makes a stack for one cell in the worker with the fewest; nil if no
// worker is up (the caller tries again later).
func (s *Supervisor) NewStack(env stack.Env, cfg stack.Config, clock func() uint64) stack.Stack {
	for range 2 { // a worker may end between the pick and the call
		r := s.pick()
		if r == nil {
			return nil
		}
		if st := r.NewStack(env, cfg, clock); st != nil {
			return st
		}
	}
	return nil
}

func (s *Supervisor) pick() *Remote {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *Remote
	for _, w := range s.workers {
		if w.rem == nil || !w.rem.Alive() {
			continue
		}
		if best == nil || w.rem.Stacks() < best.Stacks() {
			best = w.rem
		}
	}
	return best
}

// watch keeps one worker running.
func (s *Supervisor) watch(ctx context.Context, w *slot) {
	back := def(s.BackoffMin, time.Second)
	for ctx.Err() == nil {
		up := time.Now()
		err := s.runOnce(ctx, w)
		if ctx.Err() != nil {
			return
		}
		s.log().Error("a DECT worker ended: its cells' stacks restart", "worker", w.n, "why", err, "restart_in", back)
		if time.Since(up) > def(s.StableAfter, 5*time.Minute) {
			back = def(s.BackoffMin, time.Second)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(back):
		}
		back = min(2*back, def(s.BackoffMax, time.Minute))
	}
}

// runOnce starts the worker process, serves it, and returns why it ended.
func (s *Supervisor) runOnce(ctx context.Context, w *slot) error {
	exe := s.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	args := s.Args
	if args == nil {
		args = []string{"cellworker"}
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	mine, theirs := os.NewFile(uintptr(fds[0]), "worker-ctl"), os.NewFile(uintptr(fds[1]), "worker")
	conn, err := net.FileConn(mine)
	mine.Close()
	if err != nil {
		theirs.Close()
		return err
	}
	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // G204: our own binary (os.Executable) or the one the operator configured; no user input
	cmd.Env = append([]string{}, s.Env...)
	cmd.ExtraFiles = []*os.File{theirs} // file 3
	crash := &crashLog{}
	cmd.Stderr = crash
	setpdeathsig(cmd)
	if err := cmd.Start(); err != nil {
		theirs.Close()
		conn.Close()
		return err
	}
	theirs.Close()
	rem := NewRemote(conn)
	s.mu.Lock()
	w.rem = rem
	s.mu.Unlock()
	s.log().Info("a DECT worker started", "worker", w.n, "pid", cmd.Process.Pid)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	every, timeout := def(s.PingEvery, 2*time.Second), def(s.PingTimeout, 2*time.Second)
	tick := time.NewTicker(every)
	defer tick.Stop()
	var why, exitErr error
	gotExit := false
loop:
	for {
		select {
		case <-ctx.Done():
			why = ctx.Err()
			break loop
		case exitErr = <-exited:
			gotExit = true
			why = errors.New("the process ended")
			break loop
		case <-rem.Gone():
			why = errors.New("its connection ended")
			break loop
		case <-tick.C:
			if !rem.Ping(timeout) {
				why = errors.New("it did not answer a ping")
				break loop
			}
		}
	}
	s.mu.Lock()
	w.rem = nil
	s.mu.Unlock()
	rem.Close()
	_ = cmd.Process.Kill() // a hung worker; for one that ended this fails and nobody minds
	if !gotExit {
		select {
		case exitErr = <-exited:
		case <-time.After(5 * time.Second):
		}
	}
	if t := crash.text(); t != "" && ctx.Err() == nil {
		s.log().Warn("a DECT worker's last words", "worker", w.n, "text", t)
	}
	if exitErr != nil && ctx.Err() == nil {
		why = fmt.Errorf("%w: %w", why, exitErr) // "signal: segmentation fault", "exit status 2"
	}
	return why
}

// crashLog keeps what a worker writes to its stderr (a crash report, a Go panic) so
// that it is logged once, when the worker has ended, not in the fragments it was written in.
type crashLog struct {
	mu  sync.Mutex
	buf []byte
}

const crashLogMax = 8 << 10

func (c *crashLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := crashLogMax - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (c *crashLog) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}
