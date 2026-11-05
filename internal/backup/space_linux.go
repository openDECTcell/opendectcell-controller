// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package backup

import "syscall"

// FreeSpace is the free and the total space, in bytes, of the file system dir is on (for a user who is not root: what a write can use).
func FreeSpace(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize) //nolint:gosec // G115: a block size is positive
	return st.Bavail * bs, st.Blocks * bs, nil
}
