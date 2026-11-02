// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package worker

import "os/exec"

func setpdeathsig(*exec.Cmd) {}

// Harden does nothing off Linux.
func Harden() {}

// Confine does nothing off Linux.
func Confine() error { return nil }
