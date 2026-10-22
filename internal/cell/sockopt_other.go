// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package cell

import (
	"log/slog"
	"net"
)

func tuneSocket(c net.Conn, _ *slog.Logger) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
}
