//go:build !linux

package worker

// diskFreeSpace returns the available disk space in bytes for the filesystem
// containing the given path. On non-Linux platforms, returns -1 (unsupported)
// to skip disk pressure checks.
func diskFreeSpace(_ string) int64 {
	return -1
}
