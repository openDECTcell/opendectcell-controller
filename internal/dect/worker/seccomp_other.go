// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux && !amd64 && !arm64

package worker

// confine does nothing on architectures without a filter table; the worker runs unconfined there.
func confine() error { return nil }
