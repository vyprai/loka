//go:build darwin

package vz

// #cgo CFLAGS: -x objective-c -fmodules -fobjc-arc
// #cgo LDFLAGS: -framework Virtualization -framework Foundation
// #include "vz_bridge.h"
// #include <stdlib.h>
import "C"

import (
	"fmt"
	"unsafe"
)

// VM wraps an Apple Virtualization.framework virtual machine instance.
type VM struct {
	handle unsafe.Pointer // Pointer to VZVirtualMachine
	config Config
}

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

// NewVM creates a new VZ virtual machine with the given configuration.
func NewVM(cfg Config) (*VM, error) {
	cKernel := C.CString(cfg.Kernel)
	defer C.free(unsafe.Pointer(cKernel))
	cCmdline := C.CString(cfg.Cmdline)
	defer C.free(unsafe.Pointer(cCmdline))
	cRootfs := C.CString(cfg.Rootfs)
	defer C.free(unsafe.Pointer(cRootfs))
	cShared := C.CString(cfg.SharedDir)
	defer C.free(unsafe.Pointer(cShared))

	var errMsg *C.char
	handle := C.vz_create_vm(
		C.int(cfg.CPUs),
		C.ulonglong(cfg.MemoryMB*1024*1024),
		cKernel, cCmdline, cRootfs, cShared,
		&errMsg,
	)
	if handle == nil {
		msg := C.GoString(errMsg)
		C.free(unsafe.Pointer(errMsg))
		return nil, fmt.Errorf("create VM: %s", msg)
	}
	return &VM{handle: handle, config: cfg}, nil
}

// Start boots the virtual machine.
func (vm *VM) Start() error {
	var errMsg *C.char
	if C.vz_start_vm(vm.handle, &errMsg) != 0 {
		msg := C.GoString(errMsg)
		C.free(unsafe.Pointer(errMsg))
		return fmt.Errorf("start VM: %s", msg)
	}
	return nil
}

// Stop shuts down the virtual machine.
func (vm *VM) Stop() error {
	C.vz_stop_vm(vm.handle)
	return nil
}

// GuestIP returns the expected guest IP address.
// VZ NAT assigns IPs in 192.168.64.0/24 range;
// the first guest typically gets 192.168.64.2.
func (vm *VM) GuestIP() string {
	return "192.168.64.2"
}

// State returns the current VM state:
// 0=stopped, 1=running, 2=paused, 3=error.
func (vm *VM) State() int {
	return int(C.vz_vm_state(vm.handle))
}
