//go:build darwin

package vm

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/vyprai/loka/pkg/vz"
)

// VZManager implements VMManager using Apple's Virtualization.framework via CGO.
type VZManager struct {
	name   string
	vm     *vz.VM
	pf     *PortForwarder
	config VMConfig
}

// NewVZManager creates a new VZManager for the given VM name.
func NewVZManager(name string) *VZManager {
	return &VZManager{
		name: name,
		pf:   NewPortForwarder(),
	}
}

// Create configures the VZ virtual machine with the given VMConfig.
func (m *VZManager) Create(config VMConfig) error {
	m.config = config

	cpus := config.CPUs
	if cpus == 0 {
		cpus = 2
	}
	mem := config.MemoryMB
	if mem == 0 {
		mem = 2048
	}

	cmdline := "console=hvc0 root=/dev/vda rw"

	vzCfg := vz.Config{
		CPUs:      cpus,
		MemoryMB:  mem,
		Kernel:    config.Kernel,
		Cmdline:   cmdline,
		Rootfs:    config.Rootfs,
		SharedDir: config.SharedDir,
	}

	vm, err := vz.NewVM(vzCfg)
	if err != nil {
		return fmt.Errorf("vz create: %w", err)
	}
	m.vm = vm
	return nil
}

// Start boots the VZ virtual machine.
func (m *VZManager) Start() error {
	if m.vm == nil {
		return fmt.Errorf("VM not created; call Create first")
	}
	return m.vm.Start()
}

// Stop shuts down the VZ virtual machine and stops all port forwards.
func (m *VZManager) Stop() error {
	m.pf.StopAll()
	if m.vm == nil {
		return nil
	}
	return m.vm.Stop()
}

// Delete stops and releases the VZ virtual machine.
func (m *VZManager) Delete() error {
	if err := m.Stop(); err != nil {
		return err
	}
	m.vm = nil
	return nil
}

// Status returns the current VM status.
func (m *VZManager) Status() (VMStatus, error) {
	if m.vm == nil {
		return VMStatusStopped, nil
	}
	state := m.vm.State()
	switch state {
	case 1: // VZVirtualMachineStateRunning
		return VMStatusRunning, nil
	case 0: // VZVirtualMachineStateStopped
		return VMStatusStopped, nil
	default:
		return VMStatusUnknown, nil
	}
}

// Exec runs a command inside the VM over SSH.
// Requires SSH to be running in the guest on port 22.
func (m *VZManager) Exec(cmd string, args ...string) (string, error) {
	ip := m.GuestIP()
	sshArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", ip),
		cmd,
	}
	sshArgs = append(sshArgs, args...)
	out, err := exec.Command("ssh", sshArgs...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// PortForward starts forwarding from hostPort to guestPort via the guest IP.
func (m *VZManager) PortForward(hostPort, guestPort int, proto string) error {
	ip := m.GuestIP()
	return m.pf.Forward(hostPort, ip, guestPort, proto)
}

// StopPortForward stops a previously started port forward.
func (m *VZManager) StopPortForward(hostPort int) error {
	return m.pf.Stop(hostPort)
}

// Name returns the VM instance name.
func (m *VZManager) Name() string {
	return m.name
}

// GuestIP returns the guest IP address assigned by VZ NAT.
func (m *VZManager) GuestIP() string {
	if m.vm != nil {
		return m.vm.GuestIP()
	}
	return "192.168.64.2"
}
