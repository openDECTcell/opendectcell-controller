// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package backup

import "errors"

// FreeSpace is not known on this system.
func FreeSpace(string) (free, total uint64, err error) {
	return 0, 0, errors.New("backup: free space is not known on this system")
}
