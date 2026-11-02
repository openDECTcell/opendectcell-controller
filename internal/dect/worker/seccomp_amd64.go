// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux && amd64

package worker

import "golang.org/x/sys/unix"

// nativeAudit is AUDIT_ARCH_X86_64.
const nativeAudit = 0xc000003e

// deniedSyscalls are the calls the worker's job never needs and a taken-over worker would: opening and
// creating files, starting programs, making or binding sockets, reaching into other processes, and
// changing the file system view.
func deniedSyscalls() []uint32 {
	return []uint32{
		unix.SYS_OPEN, unix.SYS_OPENAT, unix.SYS_OPENAT2, unix.SYS_CREAT, unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT,
		unix.SYS_EXECVE, unix.SYS_EXECVEAT,
		unix.SYS_SOCKET, unix.SYS_CONNECT, unix.SYS_BIND, unix.SYS_LISTEN,
		unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_OPEN, unix.SYS_PIDFD_GETFD, unix.SYS_KCMP,
		unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT, unix.SYS_SETNS, unix.SYS_UNSHARE,
	}
}
