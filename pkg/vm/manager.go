package vm

// VMManager is the interface that all VM backends implement.
type VMManager interface {
	// Lifecycle
	Create(config VMConfig) error
	Start() error
	Stop() error
	Delete() error
	Status() (VMStatus, error)

	// Exec runs a command inside the VM and returns combined output.
	Exec(cmd string, args ...string) (string, error)

	// Port forwarding
	PortForward(hostPort, guestPort int, proto string) error
	StopPortForward(hostPort int) error

	// Info
	Name() string
	GuestIP() string
}

// VMConfig holds the configuration for creating a VM.
type VMConfig struct {
	Name      string
	CPUs      int
	MemoryMB  int
	DiskGB    int
	Kernel    string // Path to vmlinuz
	Initrd    string // Path to initrd
	Rootfs    string // Path to rootfs disk image
	SharedDir string // Host directory to share via virtiofs
}

// VMStatus represents the current state of a VM.
type VMStatus string

const (
	VMStatusRunning  VMStatus = "running"
	VMStatusStopped  VMStatus = "stopped"
	VMStatusCreating VMStatus = "creating"
	VMStatusUnknown  VMStatus = "unknown"
)

// NewManager creates the appropriate VMManager for the current platform.
// On macOS: returns LimaManager (wraps limactl).
// On Linux: returns DirectManager (no VM needed).
// Implemented in manager_darwin.go and manager_linux.go.
