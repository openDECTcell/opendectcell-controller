// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import "time"

// FastTimers shortens the registration timers for tests.
func FastTimers() {
	backoffMin = 200 * time.Millisecond
	backoffMax = time.Second
	authFailedRetry = time.Second
	keepaliveEvery = 300 * time.Millisecond
	requestTimeout = 700 * time.Millisecond
	unregisterWithin = 500 * time.Millisecond
}
