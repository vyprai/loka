package vm

import (
	"fmt"
	"os/exec"
	"strings"
)

// LimaManager wraps limactl to manage a Lima VM.
type LimaManager struct {
	name    string
	limactl string // path to limactl binary
	config  VMConfig
	pf      *PortForwarder
}

// NewLimaManager creates a LimaManager, locating the limactl binary.
func NewLimaManager(name string) (*LimaManager, error) {
	limactl, err := exec.LookPath("limactl")
	if err != nil {
		return nil, fmt.Errorf("limactl not found: install Lima first:\n  curl -fsSL https://vyprai.github.io/loka/install.sh | bash")
	}
	return &LimaManager{
		name:    name,
		limactl: limactl,
		pf:      NewPortForwarder(),
	}, nil
}

// Create generates a lima.yaml and creates the VM instance.
func (m *LimaManager) Create(config VMConfig) error {
	m.config = config

	// Check if instance already exists.
	out, _ := exec.Command(m.limactl, "list", "-q").Output()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == m.name {
			return nil // already exists
		}
	}

	// Generate lima.yaml config.
	yaml := m.generateYAML(config)

	// Create via stdin.
	cmd := exec.Command(m.limactl, "create", "--name="+m.name, "--tty=false", "-")
	cmd.Stdin = strings.NewReader(yaml)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("limactl create failed: %s: %w", string(output), err)
	}
	return nil
}

// Start starts the Lima VM.
func (m *LimaManager) Start() error {
	// Check if already running.
	status, err := m.Status()
	if err != nil {
		return err
	}
	if status == VMStatusRunning {
		return nil
	}

	cmd := exec.Command(m.limactl, "start", m.name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("limactl start failed: %s: %w", string(output), err)
	}
	return nil
}

// Stop stops the Lima VM.
func (m *LimaManager) Stop() error {
	m.pf.StopAll()

	cmd := exec.Command(m.limactl, "stop", "--force", m.name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("limactl stop failed: %s: %w", string(output), err)
	}
	return nil
}

// Delete deletes the Lima VM.
func (m *LimaManager) Delete() error {
	m.pf.StopAll()

	cmd := exec.Command(m.limactl, "delete", "--force", m.name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("limactl delete failed: %s: %w", string(output), err)
	}
	return nil
}

// Status returns the current VM status.
func (m *LimaManager) Status() (VMStatus, error) {
	out, err := exec.Command(m.limactl, "list", "--format", "{{.Status}}", m.name).Output()
	if err != nil {
		return VMStatusUnknown, err
	}
	s := strings.TrimSpace(string(out))
	switch s {
	case "Running":
		return VMStatusRunning, nil
	case "Stopped":
		return VMStatusStopped, nil
	default:
		return VMStatusUnknown, nil
	}
}

// Exec runs a command inside the Lima VM and returns combined output.
func (m *LimaManager) Exec(cmd string, args ...string) (string, error) {
	shellArgs := []string{"shell", m.name, "--"}
	shellArgs = append(shellArgs, cmd)
	shellArgs = append(shellArgs, args...)
	out, err := exec.Command(m.limactl, shellArgs...).CombinedOutput()
	return string(out), err
}

// PortForward sets up userspace port forwarding to the VM.
// Lima also handles port forwarding via its own config, but this provides
// runtime forwarding for ports not declared at creation time.
func (m *LimaManager) PortForward(hostPort, guestPort int, proto string) error {
	ip := m.GuestIP()
	return m.pf.Forward(hostPort, ip, guestPort, proto)
}

// StopPortForward stops a previously started port forward.
func (m *LimaManager) StopPortForward(hostPort int) error {
	return m.pf.Stop(hostPort)
}

// Name returns the VM instance name.
func (m *LimaManager) Name() string {
	return m.name
}

// GuestIP returns the guest IP. Lima VMs forward to localhost.
func (m *LimaManager) GuestIP() string {
	return "127.0.0.1"
}

// generateYAML builds a Lima YAML config from VMConfig.
func (m *LimaManager) generateYAML(config VMConfig) string {
	cpus := config.CPUs
	if cpus == 0 {
		cpus = 2
	}
	mem := config.MemoryMB
	if mem == 0 {
		mem = 2048
	}
	disk := config.DiskGB
	if disk == 0 {
		disk = 20
	}

	yaml := fmt.Sprintf(`cpus: %d
memory: %dMiB
disk: %dGiB
`, cpus, mem, disk)

	if config.SharedDir != "" {
		yaml += fmt.Sprintf(`mounts:
  - location: %q
    writable: true
`, config.SharedDir)
	}

	// Forward standard LOKA ports.
	yaml += `portForwards:
  - guestPort: 6840
    hostPort: 6840
  - guestPort: 6841
    hostPort: 6841
`
	return yaml
}
