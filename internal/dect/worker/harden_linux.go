// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package worker

import (
	"os/exec"
	"syscall"
)

// setpdeathsig makes a worker die with the controller.
func setpdeathsig(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// Harden is the worker's first act: no core file (it would hold the keys of the
// records), and not dumpable by other processes.
func Harden() {
	_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0})
	const prSetDumpable = 4
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0)
}

// Confine is the worker's second act, once it holds its socket: it gives up, for good and on every thread,
// the right to open files, run programs, make connections and reach into other processes (seccomp,
// seccomp_linux.go). An error means it could not be applied and the worker should not run: one that thinks
// it is confined and is not is worse than none.
func Confine() error { return confine() }
