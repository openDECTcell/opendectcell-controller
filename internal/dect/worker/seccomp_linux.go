// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux && (amd64 || arm64)

package worker

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// A seccomp filter for the worker. The worker is the controller's own binary under the controller's uid,
// so without it a worker taken over through a flaw in the C library could read the master key and the
// database URL in /secrets, open a connection to the database, or read the controller's memory. Once the
// worker has its socket (file 3) it needs none of that: the filter makes the calls that open files, make
// connections, start programs or reach into other processes fail with EPERM, on every thread (TSYNC).
//
// It is a deny-list on purpose: an allow-list of what the Go runtime and the C library need would
// break with each new runtime. What is denied is exactly what the worker's job never needs, and the
// one thing a deny-list cannot do, deny a system call that does not exist yet, is covered by the
// closed set of ways to reach a file or a socket.

// BPF instruction classes and fields (linux/bpf_common.h, linux/filter.h).
const (
	bpfLD                  = 0x00
	bpfW                   = 0x00
	bpfABS                 = 0x20
	bpfJMP                 = 0x05
	bpfJEQ                 = 0x10
	bpfK                   = 0x00
	bpfRET                 = 0x06
	seccompRetAllow        = 0x7fff0000
	seccompRetErrno        = 0x00050000
	seccompSetModeFilter   = 1
	seccompFilterFlagTSYNC = 1
	seccompDataNr          = 0 // offsetof(struct seccomp_data, nr)
	seccompDataArch        = 4 // offsetof(struct seccomp_data, arch)
)

type sockFilter struct {
	code uint16
	jt   uint8
	jf   uint8
	k    uint32
}

type sockFprog struct {
	len    uint16
	filter *sockFilter
}

// buildFilter assembles the program: a call made through another system call ABI (32-bit on x86-64) is
// refused whole, a denied number fails with EPERM, the rest passes.
func buildFilter(arch uint32, denied []uint32) []sockFilter {
	p := []sockFilter{
		{bpfLD | bpfW | bpfABS, 0, 0, seccompDataArch},
		{bpfJMP | bpfJEQ | bpfK, 1, 0, arch}, // native: skip the next one
		{bpfRET | bpfK, 0, 0, seccompRetErrno | uint32(unix.EPERM)},
		{bpfLD | bpfW | bpfABS, 0, 0, seccompDataNr},
	}
	for i, nr := range denied {
		// a match jumps over the remaining compares and the ALLOW to the final ERRNO
		left := uint8(len(denied) - 1 - i)
		p = append(p, sockFilter{bpfJMP | bpfJEQ | bpfK, left + 1, 0, nr})
	}
	return append(p,
		sockFilter{bpfRET | bpfK, 0, 0, seccompRetAllow},
		sockFilter{bpfRET | bpfK, 0, 0, seccompRetErrno | uint32(unix.EPERM)})
}

// confine installs the filter on every thread of the process. It must be called after the worker has
// opened what it needs.
func confine() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	prog := buildFilter(nativeAudit, deniedSyscalls())
	fp := sockFprog{len: uint16(len(prog)), filter: &prog[0]}
	// the kernel reads the program through this pointer during the call: it points at fp, which lives until the call returns
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, seccompSetModeFilter, seccompFilterFlagTSYNC, uintptr(unsafe.Pointer(&fp))) //nolint:gosec // G103: the seccomp ABI takes a pointer to a struct sock_fprog
	if errno != 0 {
		return errno
	}
	return nil
}
