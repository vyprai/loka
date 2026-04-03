//go:build linux

package lokavm

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/vyprai/loka/pkg/lokavm/virtio"
)

// KVM ioctl numbers (from linux/kvm.h).
const (
	kvmGetAPIVersion    = 0xAE00
	kvmCreateVM         = 0xAE01
	kvmCheckExtension   = 0xAE03
	kvmGetVCPUMmapSize  = 0xAE04
	kvmCreateVCPU       = 0xAE41
	kvmSetUserMemRegion = 0x4020AE46
	kvmRun              = 0xAE80
	kvmGetRegs          = 0x8090AE81
	kvmSetRegs          = 0x4090AE82
	kvmGetSRegs         = 0x8138AE83
	kvmSetSRegs         = 0x4138AE84
	kvmCreateIRQChip    = 0xAE60
	kvmSetIRQLine       = 0x4008AE61
	kvmIRQLineStatus    = 0xC008AE67

	// Exit reasons.
	kvmExitIO       = 2
	kvmExitMMIO     = 6
	kvmExitShutdown = 8
	kvmExitIntr     = 10
)

// kvmUserspaceMemRegion is KVM_SET_USER_MEMORY_REGION.
type kvmUserspaceMemRegion struct {
	Slot          uint32
	Flags         uint32
	GuestPhysAddr uint64
	MemorySize    uint64
	UserspaceAddr uint64
}

// KVMHypervisor implements the Hypervisor interface using direct KVM ioctls.
//
// STATUS: Skeleton implementation. Not production-ready for ARM64.
// On Linux, lokad uses Cloud Hypervisor (CHHypervisor) as the primary backend.
// This KVM skeleton is kept for future development of a pure-Go VMM.
//
// Missing for ARM64:
//   - vGIC setup (ARM64 uses GIC, not x86 IRQ chip)
//   - Device tree (DTB) generation for virtio device discovery
//   - ARM64 register initialization (PC, X0=DTB addr, PSTATE)
//   - MMIO dispatch to virtio devices
type KVMHypervisor struct {
	config    HypervisorConfig
	logger    *slog.Logger
	kvmFD     int // /dev/kvm file descriptor.
	runSize   int // Size of kvm_run mmap region.

	mu  sync.RWMutex
	vms map[string]*kvmVM
}

type kvmVM struct {
	config VMConfig
	state  VMState
	booted time.Time

	vmFD  int      // KVM VM file descriptor.
	vcpus []*kvmVCPU
	mem   []byte   // Guest physical memory (mmap'd).
	memFD int      // Anonymous file descriptor for memory.

	// Virtio devices.
	devices []*virtio.MMIODevice
	vsock   *virtio.Vsock

	cancel chan struct{}
}

type kvmVCPU struct {
	fd      int    // vCPU file descriptor.
	runData []byte // mmap'd kvm_run structure.
}

// NewKVMHypervisor creates a new KVM-based hypervisor for Linux.
func NewKVMHypervisor(config HypervisorConfig, logger *slog.Logger) (*KVMHypervisor, error) {
	if logger == nil {
		logger = slog.Default()
	}

	kvmFD, err := syscall.Open("/dev/kvm", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/kvm: %w", err)
	}

	// Check API version (must be 12).
	version, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(kvmFD), kvmGetAPIVersion, 0)
	if errno != 0 || version != 12 {
		syscall.Close(kvmFD)
		return nil, fmt.Errorf("KVM API version %d (expected 12), errno=%v", version, errno)
	}

	// Get vCPU mmap size.
	runSize, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(kvmFD), kvmGetVCPUMmapSize, 0)
	if errno != 0 {
		syscall.Close(kvmFD)
		return nil, fmt.Errorf("get vcpu mmap size: %v", errno)
	}

	return &KVMHypervisor{
		config:  config,
		logger:  logger,
		kvmFD:   kvmFD,
		runSize: int(runSize),
		vms:     make(map[string]*kvmVM),
	}, nil
}

func (h *KVMHypervisor) CreateVM(config VMConfig) (*VM, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, exists := h.vms[config.ID]; exists {
		return nil, fmt.Errorf("VM %s already exists", config.ID)
	}

	kvm, err := h.buildKVM(config)
	if err != nil {
		return nil, fmt.Errorf("build KVM VM: %w", err)
	}

	kvm.state = VMStateCreated
	h.vms[config.ID] = kvm

	return h.toVM(kvm), nil
}

func (h *KVMHypervisor) StartVM(id string) error {
	h.mu.Lock()
	kvm, ok := h.vms[id]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("VM %s not found", id)
	}

	kvm.state = VMStateStarting

	// Load kernel into guest memory.
	if err := h.loadKernel(kvm); err != nil {
		kvm.state = VMStateStopped
		return fmt.Errorf("load kernel: %w", err)
	}

	// Start vCPU threads.
	for i, vcpu := range kvm.vcpus {
		go h.runVCPU(kvm, vcpu, i)
	}

	kvm.state = VMStateRunning
	kvm.booted = time.Now()
	h.logger.Info("KVM VM started", "id", id, "vcpus", len(kvm.vcpus))

	return nil
}

func (h *KVMHypervisor) StopVM(id string) error {
	h.mu.Lock()
	kvm, ok := h.vms[id]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("VM %s not found", id)
	}

	close(kvm.cancel)
	kvm.state = VMStateStopped

	for _, vcpu := range kvm.vcpus {
		syscall.Close(vcpu.fd)
	}
	syscall.Close(kvm.vmFD)

	h.logger.Info("KVM VM stopped", "id", id)
	return nil
}

func (h *KVMHypervisor) DeleteVM(id string) error {
	if err := h.StopVM(id); err != nil {
		// Ignore stop errors on delete.
	}
	h.mu.Lock()
	delete(h.vms, id)
	h.mu.Unlock()
	return nil
}

func (h *KVMHypervisor) ListVMs() ([]*VM, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]*VM, 0, len(h.vms))
	for _, kvm := range h.vms {
		result = append(result, h.toVM(kvm))
	}
	return result, nil
}

func (h *KVMHypervisor) PauseVM(id string) error {
	h.mu.RLock()
	kvm, ok := h.vms[id]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("VM %s not found", id)
	}
	kvm.state = VMStatePaused
	return nil
}

func (h *KVMHypervisor) ResumeVM(id string) error {
	h.mu.RLock()
	kvm, ok := h.vms[id]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("VM %s not found", id)
	}
	kvm.state = VMStateRunning
	return nil
}

func (h *KVMHypervisor) CreateSnapshot(id string) (Snapshot, error) {
	h.mu.RLock()
	kvm, ok := h.vms[id]
	h.mu.RUnlock()
	if !ok {
		return Snapshot{}, fmt.Errorf("VM %s not found", id)
	}

	kvm.state = VMStatePaused

	memPath := fmt.Sprintf("%s/vms/%s/snapshot.mem", h.config.DataDir, id)
	if err := os.WriteFile(memPath, kvm.mem, 0o644); err != nil {
		return Snapshot{}, fmt.Errorf("save memory: %w", err)
	}

	return Snapshot{
		MemPath:  memPath,
		UpperDir: kvm.config.UpperDir,
	}, nil
}

func (h *KVMHypervisor) RestoreSnapshot(id string, snap Snapshot) error {
	h.mu.RLock()
	kvm, ok := h.vms[id]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("VM %s not found", id)
	}

	data, err := os.ReadFile(snap.MemPath)
	if err != nil {
		return fmt.Errorf("load snapshot memory: %w", err)
	}
	copy(kvm.mem, data)

	kvm.state = VMStateRunning
	return nil
}

// buildKVM creates the KVM VM with memory, vCPUs, and virtio devices.
func (h *KVMHypervisor) buildKVM(cfg VMConfig) (*kvmVM, error) {
	vmFD, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(h.kvmFD), kvmCreateVM, 0)
	if errno != 0 {
		return nil, fmt.Errorf("create VM: %v", errno)
	}

	// Create in-kernel IRQ chip.
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, vmFD, kvmCreateIRQChip, 0)
	if errno != 0 {
		syscall.Close(int(vmFD))
		return nil, fmt.Errorf("create IRQ chip: %v", errno)
	}

	memMB := cfg.MemoryMaxMB
	if memMB < cfg.MemoryMinMB {
		memMB = cfg.MemoryMinMB
	}
	if memMB < 64 {
		memMB = 64
	}
	memSize := memMB * 1024 * 1024

	flags := syscall.MAP_PRIVATE | syscall.MAP_ANONYMOUS
	mem, err := syscall.Mmap(-1, 0, memSize, syscall.PROT_READ|syscall.PROT_WRITE, flags)
	if err != nil {
		syscall.Close(int(vmFD))
		return nil, fmt.Errorf("mmap guest memory (%dMB): %w", memMB, err)
	}

	if cfg.MemoryMinMB > 0 {
		minBytes := cfg.MemoryMinMB * 1024 * 1024
		syscall.Syscall(syscall.SYS_MADVISE, uintptr(unsafe.Pointer(&mem[0])), uintptr(minBytes), syscall.MADV_WILLNEED)
	}

	region := kvmUserspaceMemRegion{
		Slot:          0,
		Flags:         0,
		GuestPhysAddr: 0,
		MemorySize:    uint64(memSize),
		UserspaceAddr: uint64(uintptr(unsafe.Pointer(&mem[0]))),
	}
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, vmFD, kvmSetUserMemRegion, uintptr(unsafe.Pointer(&region)))
	if errno != 0 {
		syscall.Munmap(mem)
		syscall.Close(int(vmFD))
		return nil, fmt.Errorf("set user memory region: %v", errno)
	}

	numCPUs := cfg.VCPUsMin
	if numCPUs < 1 {
		numCPUs = 1
	}

	vcpus := make([]*kvmVCPU, numCPUs)
	for i := 0; i < numCPUs; i++ {
		vcpuFD, _, errno := syscall.Syscall(syscall.SYS_IOCTL, vmFD, kvmCreateVCPU, uintptr(i))
		if errno != 0 {
			for j := 0; j < i; j++ {
				syscall.Close(vcpus[j].fd)
			}
			syscall.Munmap(mem)
			syscall.Close(int(vmFD))
			return nil, fmt.Errorf("create vCPU %d: %v", i, errno)
		}

		runData, err := syscall.Mmap(int(vcpuFD), 0, h.runSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			syscall.Close(int(vcpuFD))
			syscall.Munmap(mem)
			syscall.Close(int(vmFD))
			return nil, fmt.Errorf("mmap kvm_run for vCPU %d: %w", i, err)
		}

		vcpus[i] = &kvmVCPU{fd: int(vcpuFD), runData: runData}
	}

	kvm := &kvmVM{
		config: cfg,
		vmFD:   int(vmFD),
		vcpus:  vcpus,
		mem:    mem,
		cancel: make(chan struct{}),
	}

	h.setupVirtioDevices(kvm)

	return kvm, nil
}

// setupVirtioDevices creates and registers virtio-mmio devices.
func (h *KVMHypervisor) setupVirtioDevices(kvm *kvmVM) {
	console := virtio.NewConsole(os.Stdout)
	kvm.devices = append(kvm.devices, virtio.NewMMIODevice(console))

	for _, drive := range kvm.config.Drives {
		blk, err := virtio.NewBlock(drive.Path, drive.ReadOnly)
		if err != nil {
			h.logger.Warn("failed to create block device", "drive", drive.ID, "error", err)
			continue
		}
		kvm.devices = append(kvm.devices, virtio.NewMMIODevice(blk))
	}

	if kvm.config.Vsock {
		vsock := virtio.NewVsock(3)
		kvm.vsock = vsock
		kvm.devices = append(kvm.devices, virtio.NewMMIODevice(vsock))
	}

	balloon := virtio.NewBalloon()
	kvm.devices = append(kvm.devices, virtio.NewMMIODevice(balloon))
}

// loadKernel loads the Linux kernel image into guest memory.
func (h *KVMHypervisor) loadKernel(kvm *kvmVM) error {
	kernelPath := h.config.KernelPath
	if kernelPath == "" {
		return fmt.Errorf("no kernel path configured")
	}

	kernel, err := os.ReadFile(kernelPath)
	if err != nil {
		return fmt.Errorf("read kernel: %w", err)
	}

	const kernelLoadAddr = 0x200000
	if kernelLoadAddr+len(kernel) > len(kvm.mem) {
		return fmt.Errorf("kernel too large for guest memory (%d bytes)", len(kernel))
	}
	copy(kvm.mem[kernelLoadAddr:], kernel)

	return nil
}

// runVCPU is the main loop for a vCPU thread.
func (h *KVMHypervisor) runVCPU(kvm *kvmVM, vcpu *kvmVCPU, cpuID int) {
	for {
		select {
		case <-kvm.cancel:
			return
		default:
		}

		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(vcpu.fd), kvmRun, 0)
		if errno != 0 {
			if errno == syscall.EINTR {
				continue
			}
			h.logger.Error("KVM_RUN failed", "cpu", cpuID, "errno", errno)
			return
		}

		exitReason := *(*uint32)(unsafe.Pointer(&vcpu.runData[0]))

		switch exitReason {
		case kvmExitIO:
			h.handleIO(kvm, vcpu)
		case kvmExitMMIO:
			h.handleMMIO(kvm, vcpu)
		case kvmExitShutdown:
			h.logger.Info("VM shutdown", "cpu", cpuID)
			kvm.state = VMStateStopped
			return
		case kvmExitIntr:
			continue
		default:
			h.logger.Warn("unhandled KVM exit", "reason", exitReason, "cpu", cpuID)
		}
	}
}

func (h *KVMHypervisor) handleIO(kvm *kvmVM, vcpu *kvmVCPU) {
	// Read I/O exit data from the vcpu run structure.
	// Port I/O exits indicate the guest accessed an I/O port that is not
	// handled by an in-kernel device. Log for debugging; dispatch to virtio
	// devices when PIO-based transport is implemented.
	h.logger.Debug("unhandled KVM I/O exit", "vm", kvm.config.ID)
}

func (h *KVMHypervisor) handleMMIO(kvm *kvmVM, vcpu *kvmVCPU) {
	// MMIO exits indicate the guest accessed a memory-mapped region that is not
	// handled by an in-kernel device. Virtio MMIO transport dispatches here.
	h.logger.Debug("unhandled KVM MMIO exit", "vm", kvm.config.ID)
}

func (h *KVMHypervisor) toVM(kvm *kvmVM) *VM {
	vm := &VM{
		ID:     kvm.config.ID,
		Config: kvm.config,
		State:  kvm.state,
		Booted: kvm.booted,
	}

	if kvm.vsock != nil {
		vm.DialVsock = func(port uint32) (net.Conn, error) {
			return kvm.vsock.Connect(port)
		}
	}

	return vm
}
