// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !interop

package sip_test

import "github.com/openDECTcell/opendectcell-controller/internal/sip"

// Unit tests run with short timers; the interop build keeps the real ones,
// because a real PBX takes longer to answer than the fake one.
func init() { sip.FastTimers() }
