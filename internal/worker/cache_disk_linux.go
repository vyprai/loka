//go:build linux

package worker

import "syscall"

// diskFreeSpace returns the available disk space in bytes for the filesystem
// containing the given path. Returns -1 if the check fails.
func diskFreeSpace(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return -1
	}
	return int64(stat.Bavail) * int64(stat.Bsize)
}
