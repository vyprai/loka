//go:build linux

package supervisor

import "syscall"

func statfs(path string, stat *syscallStatfs) error {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return err
	}
	stat.Blocks = s.Blocks
	stat.Bfree = s.Bfree
	stat.Bsize = s.Bsize
	return nil
}
