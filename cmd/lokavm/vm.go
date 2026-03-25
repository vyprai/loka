//go:build darwin

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/vyprai/loka/pkg/vz"
)

// VM wraps a VZ virtual machine with convenience methods for lokavm.
type VM struct {
	vzVM   *vz.VM
	config vz.Config
}

func bootVM(ctx context.Context, kernelPath, initrdPath, rootfsPath string, cpus, memoryMB int, dataDir string, logger *slog.Logger) (*VM, error) {
	cfg := vz.Config{
		CPUs:      cpus,
		MemoryMB:  memoryMB,
		Kernel:    kernelPath,
		Cmdline:   "console=hvc0 root=/dev/vda rw modules=virtio_net,virtio_blk init=/sbin/init ip=dhcp",
		Initrd:    initrdPath,
		Rootfs:    rootfsPath,
		SharedDir: dataDir,
		VsockPort: 6840,
	}

	vzVM, err := vz.NewVM(cfg)
	if err != nil {
		return nil, fmt.Errorf("create VZ VM: %w", err)
	}

	if err := vzVM.Start(); err != nil {
		return nil, fmt.Errorf("start VZ VM: %w", err)
	}

	logger.Info("VZ VM started", "cpus", cpus, "memory_mb", memoryMB)
	return &VM{vzVM: vzVM, config: cfg}, nil
}

// Stop shuts down the virtual machine.
func (vm *VM) Stop() {
	vm.vzVM.Stop()
}

// GuestIP returns the NAT IP address of the guest.
func (vm *VM) GuestIP() string {
	return vm.vzVM.GuestIP()
}

// DialGuest connects to a TCP port on the guest via its NAT IP.
func (vm *VM) DialGuest(port int) (net.Conn, error) {
	addr := net.JoinHostPort(vm.GuestIP(), fmt.Sprintf("%d", port))
	return net.DialTimeout("tcp", addr, 5*time.Second)
}

// DialVsock connects to a vsock port inside the guest.
// Falls back to TCP over NAT IP if vsock is not available.
func (vm *VM) DialVsock(port uint32) (net.Conn, error) {
	// Use vsock via the VZ bridge if available.
	conn, err := vm.vzVM.DialVsock(port)
	if err == nil {
		return conn, nil
	}
	// Fallback: dial the guest's NAT IP over TCP.
	return vm.DialGuest(int(port))
}

// waitForLokad polls the VM until lokad is reachable on port 6840.
func waitForLokad(vm *VM, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("lokad did not become ready within %s", timeout)
		case <-ticker.C:
			conn, err := vm.DialGuest(6840)
			if err != nil {
				continue
			}
			conn.Close()
			return nil // lokad is listening
		}
	}
}
