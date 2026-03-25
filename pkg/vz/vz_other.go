//go:build !darwin

package vz

import "fmt"

// VM is a stub for non-macOS platforms.
type VM struct{}

// Config holds configuration for creating a VZ virtual machine.
type Config struct {
	CPUs      int
	MemoryMB  int
	Kernel    string
	Cmdline   string
	Rootfs    string
	SharedDir string
	VsockPort uint32
}

// NewVM returns an error on non-macOS platforms.
func NewVM(cfg Config) (*VM, error) {
	return nil, fmt.Errorf("Apple Virtualization Framework is only available on macOS")
}

// Start is not available on non-macOS platforms.
func (vm *VM) Start() error { return fmt.Errorf("not available") }

// Stop is not available on non-macOS platforms.
func (vm *VM) Stop() error { return fmt.Errorf("not available") }

// GuestIP returns an empty string on non-macOS platforms.
func (vm *VM) GuestIP() string { return "" }

// State returns -1 on non-macOS platforms.
func (vm *VM) State() int { return -1 }
