//go:build linux

package main

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"syscall"
)

// hasKernelParam checks if a parameter is present in /proc/cmdline.
func hasKernelParam(param string) bool {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), param)
}

// setupOverlayFS mounts the layer-pack drive and sets up overlayfs, then
// pivot_roots into the merged filesystem. This is called when the kernel
// boot args contain "loka.layers=true".
//
// Drive layout:
//   - /dev/vda — small writable ext4 (rootfs drive, mounted as / by kernel)
//   - /dev/vdb — read-only layer-pack ext4 containing numbered layer dirs
func setupOverlayFS(logger *slog.Logger) {
	logger.Info("setting up overlayfs from layers")

	// /dev/vda is already mounted as / by the kernel (writable overlay drive).
	// /dev/vdb is the read-only layer pack.

	// Mount the layer pack read-only.
	if err := os.MkdirAll("/layers", 0755); err != nil {
		logger.Error("failed to create /layers", "error", err)
		return
	}
	if err := syscall.Mount("/dev/vdb", "/layers", "ext4", syscall.MS_RDONLY, ""); err != nil {
		logger.Error("failed to mount layer pack", "error", err)
		return
	}

	// Create overlay dirs on the writable rootfs.
	for _, dir := range []string{"/upper", "/work", "/merged"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			logger.Error("failed to create dir", "dir", dir, "error", err)
			return
		}
	}

	// Find layer directories in /layers/ (numbered 0, 1, 2, ...).
	entries, err := os.ReadDir("/layers")
	if err != nil {
		logger.Error("failed to read /layers", "error", err)
		return
	}
	var layerDirs []string
	for _, e := range entries {
		if e.IsDir() {
			layerDirs = append(layerDirs, "/layers/"+e.Name())
		}
	}
	if len(layerDirs) == 0 {
		logger.Error("no layer directories found in /layers")
		return
	}
	// Sort descending — top layer first for overlayfs lowerdir order.
	sort.Sort(sort.Reverse(sort.StringSlice(layerDirs)))

	lowerdir := strings.Join(layerDirs, ":")

	// Mount overlayfs.
	opts := fmt.Sprintf("lowerdir=%s,upperdir=/upper,workdir=/work", lowerdir)
	if err := syscall.Mount("overlay", "/merged", "overlay", 0, opts); err != nil {
		logger.Error("overlayfs mount failed", "error", err, "opts", opts)
		return
	}

	// Pivot root to the merged filesystem.
	if err := os.MkdirAll("/merged/.old-root", 0755); err != nil {
		logger.Error("failed to create .old-root", "error", err)
		return
	}
	if err := syscall.PivotRoot("/merged", "/merged/.old-root"); err != nil {
		logger.Error("pivot_root failed", "error", err)
		return
	}

	// Chdir to new root.
	if err := os.Chdir("/"); err != nil {
		logger.Error("chdir to / failed", "error", err)
		return
	}

	// Unmount old root lazily and clean up.
	if err := syscall.Unmount("/.old-root", syscall.MNT_DETACH); err != nil {
		logger.Warn("unmount .old-root failed", "error", err)
	}
	os.RemoveAll("/.old-root")

	logger.Info("overlayfs mounted successfully", "layers", len(layerDirs))
}
