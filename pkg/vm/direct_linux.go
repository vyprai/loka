//go:build linux

package vm

import "os/exec"

// DirectManager runs commands directly on the host (Linux only, no VM needed).
type DirectManager struct {
	name string
}

// NewDirectManager creates a DirectManager for running on Linux without a VM.
func NewDirectManager(name string) *DirectManager {
	return &DirectManager{name: name}
}

func (m *DirectManager) Create(config VMConfig) error          { return nil }
func (m *DirectManager) Start() error                          { return nil }
func (m *DirectManager) Stop() error                           { return nil }
func (m *DirectManager) Delete() error                         { return nil }
func (m *DirectManager) Status() (VMStatus, error)             { return VMStatusRunning, nil }
func (m *DirectManager) PortForward(hostPort, guestPort int, proto string) error { return nil }
func (m *DirectManager) StopPortForward(hostPort int) error    { return nil }
func (m *DirectManager) Name() string                          { return m.name }
func (m *DirectManager) GuestIP() string                       { return "127.0.0.1" }

// Exec runs a command directly on the host.
func (m *DirectManager) Exec(cmd string, args ...string) (string, error) {
	out, err := exec.Command(cmd, args...).CombinedOutput()
	return string(out), err
}
