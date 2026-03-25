#import <Virtualization/Virtualization.h>
#include "vz_bridge.h"
#include <stdlib.h>
#include <string.h>

// Helper to set error message
static void set_error(char** error_msg, NSString* msg) {
    if (error_msg && msg) {
        const char* utf8 = [msg UTF8String];
        *error_msg = strdup(utf8);
    }
}

void* vz_create_vm(
    int cpus,
    unsigned long long memory_bytes,
    const char* kernel_path,
    const char* cmdline,
    const char* rootfs_path,
    const char* shared_dir,
    char** error_msg
) {
    @autoreleasepool {
        NSError* error = nil;

        // Boot loader
        NSURL* kernelURL = [NSURL fileURLWithPath:@(kernel_path)];
        VZLinuxBootLoader* bootLoader = [[VZLinuxBootLoader alloc] initWithKernelURL:kernelURL];
        bootLoader.commandLine = @(cmdline);

        // VM configuration
        VZVirtualMachineConfiguration* config = [[VZVirtualMachineConfiguration alloc] init];
        config.bootLoader = bootLoader;
        config.CPUCount = cpus;
        config.memorySize = memory_bytes;

        // Console (serial port via virtio)
        VZVirtioConsoleDeviceSerialPortConfiguration* console =
            [[VZVirtioConsoleDeviceSerialPortConfiguration alloc] init];
        console.attachment = [[VZFileHandleSerialPortAttachment alloc]
            initWithFileHandleForReading:[NSFileHandle fileHandleWithStandardInput]
            fileHandleForWriting:[NSFileHandle fileHandleWithStandardOutput]];
        config.serialPorts = @[console];

        // Rootfs disk
        NSURL* rootfsURL = [NSURL fileURLWithPath:@(rootfs_path)];
        VZDiskImageStorageDeviceAttachment* rootfsAttachment =
            [[VZDiskImageStorageDeviceAttachment alloc]
                initWithURL:rootfsURL readOnly:NO error:&error];
        if (error) {
            set_error(error_msg, [error localizedDescription]);
            return NULL;
        }
        VZVirtioBlockDeviceConfiguration* rootfsDisk =
            [[VZVirtioBlockDeviceConfiguration alloc] initWithAttachment:rootfsAttachment];
        config.storageDevices = @[rootfsDisk];

        // Network (NAT)
        VZVirtioNetworkDeviceConfiguration* networkConfig =
            [[VZVirtioNetworkDeviceConfiguration alloc] init];
        networkConfig.attachment = [[VZNATNetworkDeviceAttachment alloc] init];
        config.networkDevices = @[networkConfig];

        // Shared directory (virtiofs)
        if (shared_dir && strlen(shared_dir) > 0) {
            NSURL* dirURL = [NSURL fileURLWithPath:@(shared_dir)];
            VZSharedDirectory* sharedDir =
                [[VZSharedDirectory alloc] initWithURL:dirURL readOnly:NO];
            VZSingleDirectoryShare* share =
                [[VZSingleDirectoryShare alloc] initWithDirectory:sharedDir];
            VZVirtioFileSystemDeviceConfiguration* fsConfig =
                [[VZVirtioFileSystemDeviceConfiguration alloc] initWithTag:@"share"];
            fsConfig.share = share;
            config.directorySharingDevices = @[fsConfig];
        }

        // Vsock
        VZVirtioSocketDeviceConfiguration* vsockConfig =
            [[VZVirtioSocketDeviceConfiguration alloc] init];
        config.socketDevices = @[vsockConfig];

        // Validate
        if (![config validateWithError:&error]) {
            set_error(error_msg, [error localizedDescription]);
            return NULL;
        }

        // Create VM
        VZVirtualMachine* vm = [[VZVirtualMachine alloc] initWithConfiguration:config];

        // Retain the VM (prevent ARC dealloc)
        return (__bridge_retained void*)vm;
    }
}

int vz_start_vm(void* vm_handle, char** error_msg) {
    @autoreleasepool {
        VZVirtualMachine* vm = (__bridge VZVirtualMachine*)vm_handle;

        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        __block NSError* startError = nil;

        dispatch_async(dispatch_get_main_queue(), ^{
            [vm startWithCompletionHandler:^(NSError* error) {
                startError = error;
                dispatch_semaphore_signal(sem);
            }];
        });

        dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 30 * NSEC_PER_SEC));

        if (startError) {
            set_error(error_msg, [startError localizedDescription]);
            return -1;
        }
        return 0;
    }
}

void vz_stop_vm(void* vm_handle) {
    @autoreleasepool {
        VZVirtualMachine* vm = (__bridge VZVirtualMachine*)vm_handle;
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);

        dispatch_async(dispatch_get_main_queue(), ^{
            if (vm.canRequestStop) {
                [vm requestStopWithError:nil];
            } else {
                [vm stopWithCompletionHandler:^(NSError* error) {
                    dispatch_semaphore_signal(sem);
                }];
            }
            dispatch_semaphore_signal(sem);
        });

        dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC));
    }
}

int vz_vm_state(void* vm_handle) {
    VZVirtualMachine* vm = (__bridge VZVirtualMachine*)vm_handle;
    return (int)vm.state;
}
