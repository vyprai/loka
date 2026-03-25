package supervisor

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/vyprai/loka/internal/supervisor/fusemount"
	"github.com/vyprai/loka/internal/worker/vm"
)

// volumeManager tracks active volume mounts inside the VM.
type volumeManager struct {
	mu     sync.Mutex
	mounts []*activeMount
	logger *slog.Logger
}

type activeMount struct {
	path     string
	mode     string // "fuse" or "block"
	readOnly bool
	fuse     *fusemount.Mount // non-nil for fuse mode
}

func newVolumeManager(logger *slog.Logger) *volumeManager {
	return &volumeManager{logger: logger}
}

// mountVolume handles the mount_volume RPC inside the supervisor.
func (v *volumeManager) mountVolume(req vm.MountVolumeRequest, rpc fusemount.RPCCaller) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Check for duplicate mount at same path.
	for _, m := range v.mounts {
		if m.path == req.Path {
			return fmt.Errorf("volume already mounted at %s", req.Path)
		}
	}

	switch req.Mode {
	case "fuse":
		return v.mountFuse(req, rpc)
	case "block":
		return v.mountBlock(req)
	default:
		return fmt.Errorf("unknown mount mode: %s", req.Mode)
	}
}

// mountFuse creates a FUSE-like mount backed by vsock RPCs to the host.
func (v *volumeManager) mountFuse(req vm.MountVolumeRequest, rpc fusemount.RPCCaller) error {
	// Ensure /dev/fuse exists (needed for future raw FUSE support).
	if _, err := os.Stat("/dev/fuse"); os.IsNotExist(err) {
		// mknod /dev/fuse c 10 229
		cmd := exec.Command("mknod", "/dev/fuse", "c", "10", "229")
		cmd.Run() // best-effort
	}

	mount := fusemount.NewMount(req.Path, req.Bucket, req.Prefix, req.ReadOnly, rpc, v.logger)
	if err := mount.Start(); err != nil {
		return fmt.Errorf("start fuse mount: %w", err)
	}

	v.mounts = append(v.mounts, &activeMount{
		path:     req.Path,
		mode:     "fuse",
		readOnly: req.ReadOnly,
		fuse:     mount,
	})

	v.logger.Info("fuse volume mounted",
		"path", req.Path,
		"bucket", req.Bucket,
		"prefix", req.Prefix,
		"readOnly", req.ReadOnly,
	)
	return nil
}

// mountBlock mounts a pre-attached Firecracker drive (block device) at the given path.
// The drive device is determined from kernel boot args: loka.mount<N>=<dev>:<path>:<ro|rw>.
func (v *volumeManager) mountBlock(req vm.MountVolumeRequest) error {
	// Find the block device for this mount path from kernel boot args.
	dev, readOnlyFromArgs, err := findBlockDevice(req.Path)
	if err != nil {
		return fmt.Errorf("find block device for %s: %w", req.Path, err)
	}

	// Create mount point.
	if err := os.MkdirAll(req.Path, 0o755); err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}

	// Determine mount flags.
	mountOpts := ""
	if req.ReadOnly || readOnlyFromArgs {
		mountOpts = "ro"
	}

	// Mount the ext4 block device.
	args := []string{"-t", "ext4"}
	if mountOpts != "" {
		args = append(args, "-o", mountOpts)
	}
	args = append(args, "/dev/"+dev, req.Path)

	cmd := exec.Command("mount", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s at %s: %w (%s)", dev, req.Path, err, string(out))
	}

	v.mounts = append(v.mounts, &activeMount{
		path:     req.Path,
		mode:     "block",
		readOnly: req.ReadOnly || readOnlyFromArgs,
	})

	v.logger.Info("block volume mounted",
		"device", dev,
		"path", req.Path,
		"readOnly", req.ReadOnly || readOnlyFromArgs,
	)
	return nil
}

// unmountAll stops all active mounts.
func (v *volumeManager) unmountAll() {
	v.mu.Lock()
	defer v.mu.Unlock()

	for _, m := range v.mounts {
		if m.fuse != nil {
			m.fuse.Stop()
		}
		if m.mode == "block" {
			exec.Command("umount", m.path).Run()
		}
	}
	v.mounts = nil
}

// findBlockDevice reads /proc/cmdline and finds the block device for a given
// mount path. Kernel args format: loka.mount<N>=<dev>:<path>:<ro|rw>
func findBlockDevice(mountPath string) (device string, readOnly bool, err error) {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return "", false, fmt.Errorf("read /proc/cmdline: %w", err)
	}

	cmdline := string(data)
	for _, param := range strings.Fields(cmdline) {
		if !strings.HasPrefix(param, "loka.mount") {
			continue
		}
		// Format: loka.mount<N>=<dev>:<path>:<ro|rw>
		parts := strings.SplitN(param, "=", 2)
		if len(parts) != 2 {
			continue
		}
		fields := strings.SplitN(parts[1], ":", 3)
		if len(fields) != 3 {
			continue
		}
		dev, path, access := fields[0], fields[1], fields[2]
		if path == mountPath {
			return dev, access == "ro", nil
		}
		_ = dev
	}

	return "", false, fmt.Errorf("no block device found for mount path %s in kernel args", mountPath)
}

// handleMountVolume is the RPC handler for mount_volume.
func (s *Server) handleMountVolume(req vm.RPCRequest) vm.RPCResponse {
	var params vm.MountVolumeRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, fmt.Errorf("invalid params: %w", err))
	}

	// For FUSE mode, the supervisor needs an RPC caller to proxy file ops
	// back to the host. We use a no-op caller here; the actual host-side
	// file proxy is wired in via the hostRPCCaller field.
	var rpc fusemount.RPCCaller
	if s.hostRPCCaller != nil {
		rpc = s.hostRPCCaller
	}

	if err := s.volumes.mountVolume(params, rpc); err != nil {
		return rpcError(req.ID, err)
	}

	result, _ := json.Marshal(map[string]bool{"ok": true})
	return vm.RPCResponse{ID: req.ID, Result: result}
}
