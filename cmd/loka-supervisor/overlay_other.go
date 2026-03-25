//go:build !linux

package main

import "log/slog"

// hasKernelParam is a no-op on non-Linux platforms.
func hasKernelParam(_ string) bool {
	return false
}

// setupOverlayFS is a no-op on non-Linux platforms.
// Overlayfs and pivot_root are Linux-only syscalls.
func setupOverlayFS(_ *slog.Logger) {}
