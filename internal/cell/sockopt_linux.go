// SPDX-License-Identifier: AGPL-3.0-or-later

package cell

import (
	"log/slog"
	"net"

	"golang.org/x/sys/unix"
)

// notSentLowat keeps the kernel send buffer small, so the strict priority
// of the send queues is not undone in the kernel (cell-protocol.md §9.1).
const notSentLowat = 8 << 10

func tuneSocket(c net.Conn, log *slog.Logger) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetNoDelay(true) // Go's default, stated for §2 rule 2
	raw, err := tc.SyscallConn()
	if err != nil {
		return
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, notSentLowat)
	}); err == nil && serr != nil {
		log.Warn("TCP_NOTSENT_LOWAT not set", "err", serr)
	}
}
