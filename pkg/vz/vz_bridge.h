#ifndef VZ_BRIDGE_H
#define VZ_BRIDGE_H

#include <stdint.h>

// Create a VM with the given configuration. Returns an opaque handle.
void* vz_create_vm(
    int cpus,
    unsigned long long memory_bytes,
    const char* kernel_path,
    const char* cmdline,
    const char* rootfs_path,
    const char* shared_dir,
    char** error_msg
);

// Start the VM. Returns 0 on success.
int vz_start_vm(void* vm_handle, char** error_msg);

// Stop the VM.
void vz_stop_vm(void* vm_handle);

// Get VM state: 0=stopped, 1=running, 2=paused, 3=error
int vz_vm_state(void* vm_handle);

#endif
