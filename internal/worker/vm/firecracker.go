package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FirecrackerConfig holds paths to Firecracker components.
type FirecrackerConfig struct {
	BinaryPath string // Path to firecracker binary.
	KernelPath string // Path to vmlinux kernel image.
	RootfsPath string // Path to base rootfs ext4 image.
	DataDir    string // Working directory for VM sockets and state.
}

// MicroVM represents a running Firecracker microVM instance.
type MicroVM struct {
	ID         string
	SocketPath string // API socket path.
	VsockPath  string // Vsock UDS path for host-guest communication.
	PID        int    // Firecracker process PID.
	Config     VMConfig
	State      VMState
	TapName    string // TAP device name on the host (e.g. "tap3").
	GuestIP    string // Guest IP address (e.g. "172.16.0.14").

	cmd    *exec.Cmd
	cancel context.CancelFunc
	mu     sync.Mutex
	logger *slog.Logger
}

// VMConfig is the configuration for a single microVM.
type VMConfig struct {
	VCPU       int
	MemoryMB   int
	KernelPath string
	RootfsPath string
	VsockCID uint32 // Vsock guest CID (unique per VM).

	// Layered image support: read-only layer-pack ext4 with overlayfs.
	LayerPackPath string // Path to read-only layer-pack ext4.

	// Warm snapshot restore (set both for ~28ms startup instead of cold boot).
	SnapshotMemPath     string // Path to memory snapshot file.
	SnapshotVMStatePath string // Path to VM state snapshot file.

	// MountDrives are extra Firecracker drives for block-mode volume mounts.
	// Each is an ext4 image attached as /dev/vdX inside the VM.
	MountDrives []MountDrive
}

// MountDrive describes an extra ext4 drive attached to a VM for block-mode volume mounts.
type MountDrive struct {
	MountPath string // Mount path inside VM (e.g. "/data/uploads").
	HostPath  string // Path to ext4 image on the host.
	ReadOnly  bool
}

// VMState tracks the lifecycle state of a VM.
type VMState string

const (
	VMStateCreating VMState = "creating"
	VMStateRunning  VMState = "running"
	VMStatePaused   VMState = "paused"
	VMStateStopped  VMState = "stopped"
)

// Manager manages Firecracker microVM instances on this worker.
type Manager struct {
	cfg    FirecrackerConfig
	mu     sync.Mutex
	vms    map[string]*MicroVM
	nextCID uint32 // Next vsock CID to assign.
	logger *slog.Logger
}

// NewManager creates a new Firecracker VM manager.
func NewManager(cfg FirecrackerConfig, logger *slog.Logger) (*Manager, error) {
	// Validate Firecracker binary exists.
	if _, err := os.Stat(cfg.BinaryPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("firecracker binary not found at %s — run 'make fetch-firecracker'", cfg.BinaryPath)
	}
	if _, err := os.Stat(cfg.KernelPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("kernel image not found at %s — run 'make fetch-kernel'", cfg.KernelPath)
	}
	if _, err := os.Stat(cfg.RootfsPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("rootfs image not found at %s — run 'make build-rootfs'", cfg.RootfsPath)
	}

	os.MkdirAll(cfg.DataDir, 0o755)

	mgr := &Manager{
		cfg:     cfg,
		vms:     make(map[string]*MicroVM),
		nextCID: 3, // CID 0=hypervisor, 1=loopback, 2=host, 3+=guests.
		logger:  logger,
	}

	// Clean up stale TAP interfaces from previous runs.
	mgr.cleanupStaleTAPs()

	return mgr, nil
}

// DataDir returns the working directory for VM sockets and state.
func (m *Manager) DataDir() string { return m.cfg.DataDir }

// Launch starts a new Firecracker microVM.
// If cfg.SnapshotMemPath and cfg.SnapshotVMStatePath are set, restores from
// a warm snapshot (~28ms) instead of cold-booting (~1-2s).
func (m *Manager) Launch(ctx context.Context, id string, cfg VMConfig) (*MicroVM, error) {
	m.mu.Lock()
	if _, exists := m.vms[id]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("VM %s already exists", id)
	}
	cid := m.nextCID
	m.nextCID++
	m.mu.Unlock()

	cfg.VsockCID = cid
	if cfg.KernelPath == "" {
		cfg.KernelPath = m.cfg.KernelPath
	}
	if cfg.RootfsPath == "" {
		cfg.RootfsPath = m.cfg.RootfsPath
	}

	vmDir := filepath.Join(m.cfg.DataDir, "vms", id)
	os.MkdirAll(vmDir, 0o755)

	socketPath := filepath.Join(vmDir, "firecracker.sock")
	vsockPath := filepath.Join(vmDir, "vsock.sock")

	// Remove stale sockets.
	os.Remove(socketPath)
	os.Remove(vsockPath)

	// Create a TAP network interface for this VM.
	tapName := fmt.Sprintf("tap%d", cid)
	guestIP := fmt.Sprintf("172.16.0.%d", cid*4+2)
	hostIP := fmt.Sprintf("172.16.0.%d/30", cid*4+1)
	if err := createTAP(tapName, hostIP); err != nil {
		m.logger.Warn("failed to create TAP device — VM will have no network",
			"tap", tapName, "error", err)
	}

	// Determine startup mode: snapshot restore (~28ms) vs cold boot (~1-2s).
	useSnapshot := cfg.SnapshotMemPath != "" && cfg.SnapshotVMStatePath != ""

	var cmd *exec.Cmd
	vmCtx, cancel := context.WithCancel(ctx)

	if useSnapshot {
		// Snapshot restore: start Firecracker with --no-api, then load snapshot via API.
		cmd = exec.CommandContext(vmCtx, m.cfg.BinaryPath,
			"--api-sock", socketPath,
		)
	} else {
		// Cold boot: start with full config.
		fcConfig := buildFirecrackerConfig(cfg, socketPath, vsockPath)
		configPath := filepath.Join(vmDir, "config.json")
		configBytes, _ := json.MarshalIndent(fcConfig, "", "  ")
		if err := os.WriteFile(configPath, configBytes, 0o644); err != nil {
			cancel()
			return nil, fmt.Errorf("write config: %w", err)
		}
		cmd = exec.CommandContext(vmCtx, m.cfg.BinaryPath,
			"--api-sock", socketPath,
			"--config-file", configPath,
		)
	}
	cmd.Dir = vmDir
	cmd.Stdout = os.Stdout // TODO: route to structured logging.
	cmd.Stderr = os.Stderr

	microVM := &MicroVM{
		ID:         id,
		SocketPath: socketPath,
		VsockPath:  vsockPath,
		Config:     cfg,
		State:      VMStateCreating,
		TapName:    tapName,
		GuestIP:    guestIP,
		cmd:        cmd,
		cancel:     cancel,
		logger:     m.logger,
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	microVM.PID = cmd.Process.Pid
	microVM.State = VMStateRunning

	m.mu.Lock()
	m.vms[id] = microVM
	m.mu.Unlock()

	// Wait for the VM process in the background.
	go func() {
		cmd.Wait()
		m.mu.Lock()
		microVM.State = VMStateStopped
		m.mu.Unlock()
		m.logger.Info("VM process exited", "id", id, "pid", microVM.PID)
	}()

	m.logger.Info("VM launched",
		"id", id,
		"pid", microVM.PID,
		"vcpu", cfg.VCPU,
		"memory_mb", cfg.MemoryMB,
		"cid", cid,
		"tap", tapName,
		"guest_ip", guestIP,
		"socket", socketPath,
	)

	// If using snapshot restore, load the snapshot via Firecracker API.
	if useSnapshot {
		if err := waitForSocket(socketPath, 5*time.Second); err != nil {
			m.logger.Warn("firecracker API socket not ready", "id", id)
		}
		// Load snapshot via PUT /snapshot/load.
		body := fmt.Sprintf(`{"snapshot_path":"%s","mem_backend":{"backend_path":"%s","backend_type":"File"},"enable_diff_snapshots":false,"resume_vm":true}`,
			cfg.SnapshotVMStatePath, cfg.SnapshotMemPath)
		if err := firecrackerAPIPut(socketPath, "/snapshot/load", body); err != nil {
			cancel()
			return nil, fmt.Errorf("load snapshot: %w", err)
		}
		m.logger.Info("VM restored from warm snapshot", "id", id, "pid", microVM.PID)
	}

	// Wait for the vsock to be ready.
	if err := waitForSocket(vsockPath, 10*time.Second); err != nil {
		m.logger.Warn("vsock not ready within timeout — supervisor may not have started", "id", id)
	}

	return microVM, nil
}

// Stop stops a running VM.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	vm, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("VM %s not found", id)
	}
	m.mu.Unlock()

	vm.mu.Lock()
	defer vm.mu.Unlock()

	if vm.State == VMStateStopped {
		return nil
	}

	vm.cancel()
	vm.State = VMStateStopped

	// Clean up TAP device.
	if vm.TapName != "" {
		destroyTAP(vm.TapName)
	}

	// Clean up socket files.
	os.Remove(vm.SocketPath)
	os.Remove(vm.VsockPath)

	m.mu.Lock()
	delete(m.vms, id)
	m.mu.Unlock()

	m.logger.Info("VM stopped", "id", id)
	return nil
}

// Get returns a VM by ID.
func (m *Manager) Get(id string) (*MicroVM, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vm, ok := m.vms[id]
	return vm, ok
}

// List returns all VMs.
func (m *Manager) List() []*MicroVM {
	m.mu.Lock()
	defer m.mu.Unlock()
	vms := make([]*MicroVM, 0, len(m.vms))
	for _, vm := range m.vms {
		vms = append(vms, vm)
	}
	return vms
}

// Pause pauses a VM (for snapshotting).
func (m *Manager) Pause(id string) error {
	vm, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("VM %s not found", id)
	}
	// Send PATCH to Firecracker API to pause.
	return firecrackerAPIPatch(vm.SocketPath, "/vm", `{"state":"Paused"}`)
}

// Resume resumes a paused VM.
func (m *Manager) Resume(id string) error {
	vm, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("VM %s not found", id)
	}
	return firecrackerAPIPatch(vm.SocketPath, "/vm", `{"state":"Resumed"}`)
}

// CreateSnapshot creates a full VM snapshot (memory + disk state).
// The VM is paused before snapshotting and resumed afterwards.
func (m *Manager) CreateSnapshot(id, memPath, snapPath string) error {
	vm, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("VM %s not found", id)
	}

	// Pause first.
	if err := m.Pause(id); err != nil {
		return fmt.Errorf("pause for snapshot: %w", err)
	}

	body := fmt.Sprintf(`{"snapshot_type":"Full","snapshot_path":"%s","mem_file_path":"%s"}`, snapPath, memPath)
	if err := firecrackerAPIPut(vm.SocketPath, "/snapshot/create", body); err != nil {
		m.Resume(id)
		return fmt.Errorf("create snapshot: %w", err)
	}

	// Resume after snapshot.
	return m.Resume(id)
}

// CreateDiffSnapshot pauses the VM, creates a full snapshot (gzip compression
// makes it small enough), and leaves the VM paused. The caller should Stop
// the VM afterwards. Returns the paths to the memory and vmstate files.
func (m *Manager) CreateDiffSnapshot(id string) (memPath, statePath string, err error) {
	vm, ok := m.Get(id)
	if !ok {
		return "", "", fmt.Errorf("VM %s not found", id)
	}

	vmDir := filepath.Dir(vm.SocketPath)
	memPath = filepath.Join(vmDir, "snapshot_mem")
	statePath = filepath.Join(vmDir, "snapshot_vmstate")

	// Pause the VM.
	if err := m.Pause(id); err != nil {
		return "", "", fmt.Errorf("pause for snapshot: %w", err)
	}

	// Create full snapshot. (Diff requires enable_diff_snapshots before boot
	// which isn't supported in --config-file mode. Full snapshots compress
	// well with gzip since most memory pages are zeros.)
	body := fmt.Sprintf(`{"snapshot_type":"Full","snapshot_path":"%s","mem_file_path":"%s"}`, statePath, memPath)
	if err := firecrackerAPIPut(vm.SocketPath, "/snapshot/create", body); err != nil {
		return "", "", fmt.Errorf("create snapshot: %w", err)
	}

	m.logger.Info("snapshot created", "id", id, "mem", memPath, "state", statePath)
	return memPath, statePath, nil
}

// ── Firecracker Config Builder ──────────────────────────

type fcConfig struct {
	BootSource        fcBootSource         `json:"boot-source"`
	Drives            []fcDrive            `json:"drives"`
	MachineConfig     fcMachineConfig      `json:"machine-config"`
	Vsock             *fcVsock             `json:"vsock,omitempty"`
	NetworkInterfaces []fcNetworkInterface `json:"network-interfaces,omitempty"`
}

type fcNetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

type fcBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type fcDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type fcMachineConfig struct {
	VcpuCount  int `json:"vcpu_count"`
	MemSizeMib int `json:"mem_size_mib"`
}

type fcVsock struct {
	GuestCID int    `json:"guest_cid"`
	UdsPath  string `json:"uds_path"`
}

func buildFirecrackerConfig(cfg VMConfig, socketPath, vsockPath string) fcConfig {
	vcpu := cfg.VCPU
	if vcpu == 0 {
		vcpu = 1
	}
	mem := cfg.MemoryMB
	if mem == 0 {
		mem = 512
	}

	// Kernel boot args:
	// - ip=... configures the guest eth0 with a /30 subnet and loopback
	// - init= sets the supervisor as PID 1
	// The kernel ip= format: ip=client::gateway:netmask:hostname:device:autoconf
	guestIP := fmt.Sprintf("172.16.0.%d", cfg.VsockCID*4+2)
	hostIP := fmt.Sprintf("172.16.0.%d", cfg.VsockCID*4+1)
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off ip=%s::%s:255.255.255.252::eth0:off init=/usr/local/bin/loka-supervisor",
		guestIP, hostIP,
	)

	var drives []fcDrive

	if cfg.LayerPackPath != "" {
		// Layered mode: writable overlay drive + read-only layer-pack.
		// Create a sparse ext4 for the overlay writable layer (upper/work dirs).
		overlayPath := filepath.Join(filepath.Dir(socketPath), "overlay.ext4")
		exec.Command("truncate", "-s", "4G", overlayPath).Run()
		exec.Command("mkfs.ext4", "-F", "-q", overlayPath).Run()

		drives = []fcDrive{
			{DriveID: "rootfs", PathOnHost: overlayPath, IsRootDevice: true, IsReadOnly: false},
			{DriveID: "layers", PathOnHost: cfg.LayerPackPath, IsRootDevice: false, IsReadOnly: true},
		}

		// Tell the supervisor to set up overlayfs from the layer-pack.
		bootArgs += " loka.layers=true"
	} else {
		// Single rootfs mode (no layer-pack).
		drives = []fcDrive{
			{
				DriveID:      "rootfs",
				PathOnHost:   cfg.RootfsPath,
				IsRootDevice: true,
				IsReadOnly:   false,
			},
		}
	}

	// Append extra drives for block-mode volume mounts.
	// Drive letters: vda=rootfs, vdb=layers (if present), vdc+...=mounts.
	for i, md := range cfg.MountDrives {
		driveID := fmt.Sprintf("mount%d", i)
		drives = append(drives, fcDrive{
			DriveID:      driveID,
			PathOnHost:   md.HostPath,
			IsRootDevice: false,
			IsReadOnly:   md.ReadOnly,
		})
		// Pass mount metadata via kernel boot args so the supervisor knows
		// which drive to mount at which path.
		// Format: loka.mount<N>=<drive_letter>:<path>:<ro|rw>
		// Drive letters start at 'c' if layers present, 'b' otherwise.
		driveLetter := 'b' + rune(i)
		if cfg.LayerPackPath != "" {
			driveLetter = 'c' + rune(i)
		}
		access := "rw"
		if md.ReadOnly {
			access = "ro"
		}
		bootArgs += fmt.Sprintf(" loka.mount%d=vd%c:%s:%s", i, driveLetter, md.MountPath, access)
	}

	// TAP network interface: each VM gets a dedicated TAP device.
	tapName := fmt.Sprintf("tap%d", cfg.VsockCID)

	return fcConfig{
		BootSource: fcBootSource{
			KernelImagePath: cfg.KernelPath,
			BootArgs:        bootArgs,
		},
		Drives: drives,
		MachineConfig: fcMachineConfig{
			VcpuCount:  vcpu,
			MemSizeMib: mem,
		},
		Vsock: &fcVsock{
			GuestCID: int(cfg.VsockCID),
			UdsPath:  vsockPath,
		},
		NetworkInterfaces: []fcNetworkInterface{
			{IfaceID: "eth0", HostDevName: tapName},
		},
	}
}

// ── Firecracker API Helpers ─────────────────────────────

func firecrackerAPIPatch(socketPath, path, body string) error {
	return firecrackerAPICall(socketPath, "PATCH", path, body)
}

func firecrackerAPIPut(socketPath, path, body string) error {
	return firecrackerAPICall(socketPath, "PUT", path, body)
}

func firecrackerAPICall(socketPath, method, path, body string) error {
	// Use curl with unix socket to talk to Firecracker API.
	args := []string{
		"--unix-socket", socketPath,
		"-X", method,
		"-H", "Content-Type: application/json",
		"-d", body,
		"http://localhost" + path,
	}
	cmd := exec.Command("curl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (output: %s)", method, path, err, string(out))
	}
	return nil
}

// cleanupStaleTAPs removes TAP interfaces left over from previous lokad runs.
func (m *Manager) cleanupStaleTAPs() {
	out, err := exec.Command("ip", "-o", "link", "show", "type", "tun").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSuffix(parts[1], ":")
		if strings.HasPrefix(name, "tap") {
			exec.Command("ip", "link", "del", name).Run()
			m.logger.Debug("cleaned up stale TAP", "name", name)
		}
	}
}

// createTAP creates a TAP network device on the host and assigns it an IP.
// Each VM gets a /30 subnet so only the host and guest share the link.
func createTAP(tapName, hostCIDR string) error {
	// Remove any stale TAP with the same name.
	exec.Command("ip", "link", "del", tapName).Run()

	if out, err := exec.Command("ip", "tuntap", "add", tapName, "mode", "tap").CombinedOutput(); err != nil {
		return fmt.Errorf("create TAP %s: %w (%s)", tapName, err, string(out))
	}
	if out, err := exec.Command("ip", "addr", "add", hostCIDR, "dev", tapName).CombinedOutput(); err != nil {
		exec.Command("ip", "link", "del", tapName).Run()
		return fmt.Errorf("assign IP to TAP %s: %w (%s)", tapName, err, string(out))
	}
	if out, err := exec.Command("ip", "link", "set", tapName, "up").CombinedOutput(); err != nil {
		exec.Command("ip", "link", "del", tapName).Run()
		return fmt.Errorf("bring up TAP %s: %w (%s)", tapName, err, string(out))
	}
	return nil
}

// destroyTAP removes a TAP network device from the host.
func destroyTAP(tapName string) {
	exec.Command("ip", "link", "del", tapName).Run()
}

// EnableIPForwarding enables IPv4 forwarding and sets up NAT masquerading
// so VMs can reach the internet through the host. The outIface parameter
// is the host's outbound interface (e.g. "eth0", "lima0").
func EnableIPForwarding(outIface string) error {
	if out, err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").CombinedOutput(); err != nil {
		return fmt.Errorf("enable ip_forward: %w (%s)", err, string(out))
	}
	if out, err := exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING", "-o", outIface, "-j", "MASQUERADE").CombinedOutput(); err != nil {
		// Rule doesn't exist yet, add it.
		if out, err := exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "-o", outIface, "-j", "MASQUERADE").CombinedOutput(); err != nil {
			return fmt.Errorf("add MASQUERADE rule: %w (%s)", err, string(out))
		}
		_ = out
	}
	return nil
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("socket %s not ready after %s", path, timeout)
}
