package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"github.com/spf13/cobra"
)

// DeploymentFile is the YAML schema for declarative deployments.
type DeploymentFile struct {
	Name     string              `yaml:"name"`
	Provider string              `yaml:"provider"` // vm, aws, gcp, azure, do, ovh, local
	Region   string              `yaml:"region,omitempty"`
	Zone     string              `yaml:"zone,omitempty"`
	Project  string              `yaml:"project,omitempty"`

	SSH *DeploySSH `yaml:"ssh,omitempty"`

	ControlPlane *DeployCPSpec      `yaml:"controlplane,omitempty"`
	Workers      []DeployWorkerSpec `yaml:"workers,omitempty"`
}

// DeploySSH is shared SSH config for all nodes.
type DeploySSH struct {
	User string `yaml:"user"`
	Key  string `yaml:"key"`
}

// DeployCPSpec describes the control plane node.
type DeployCPSpec struct {
	Address      string `yaml:"address"`       // VM address (vm provider)
	InstanceType string `yaml:"instance_type"` // Cloud instance type
	Port         string `yaml:"port"`          // API port (default 6840)
}

// DeployWorkerSpec describes a worker node.
type DeployWorkerSpec struct {
	Address      string            `yaml:"address"`       // VM address
	InstanceType string            `yaml:"instance_type"` // Cloud instance type
	Labels       map[string]string `yaml:"labels,omitempty"`
}

func newDeployFileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply <file.yml>",
		Short: "Deploy or update from a YAML file",
		Long: `Declarative deployment — define your infrastructure in YAML and apply it.

LOKA compares the file with the current state and adds/removes workers as needed.

Examples:
  loka space apply deployment.yml
  loka space apply prod.yml

Example YAML (VM provider):
  name: production
  provider: vm
  ssh:
    user: ubuntu
    key: ~/.ssh/id_rsa
  controlplane:
    address: 10.0.0.1
  workers:
    - address: 10.0.0.2
      labels:
        gpu: "true"
    - address: 10.0.0.3
    - address: 10.0.0.4

Example YAML (local):
  name: dev
  provider: local`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]

			data, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("read %s: %w", filePath, err)
			}
			var spec DeploymentFile
			if err := yaml.Unmarshal(data, &spec); err != nil {
				return fmt.Errorf("parse %s: %w", filePath, err)
			}

			if spec.Name == "" {
				return fmt.Errorf("'name' is required in %s", filePath)
			}
			if spec.Provider == "" {
				return fmt.Errorf("'provider' is required in %s", filePath)
			}

			store, _ := loadDeployments()
			existing := store.Get(spec.Name)

			if existing == nil {
				return applyCreate(cmd, store, &spec)
			}
			return applyUpdate(cmd, store, existing, &spec)
		},
	}
}

// applyCreate handles first-time deployment from a spec file.
func applyCreate(cmd *cobra.Command, store *DeploymentStore, spec *DeploymentFile) error {
	fmt.Printf("Creating server %q (provider: %s)\n\n", spec.Name, spec.Provider)

	switch spec.Provider {
	case "local":
		return applyLocal(store, spec)
	case "vm":
		return applyVM(store, spec)
	case "aws", "gcp", "azure", "do", "ovh":
		return applyCloud(store, spec)
	default:
		return fmt.Errorf("unknown provider %q", spec.Provider)
	}
}

// applyUpdate compares existing state with spec and reconciles.
func applyUpdate(cmd *cobra.Command, store *DeploymentStore, existing *Deployment, spec *DeploymentFile) error {
	fmt.Printf("Updating server %q\n\n", spec.Name)

	if spec.Provider == "vm" {
		return reconcileVMWorkers(store, existing, spec)
	}

	if spec.Provider == "local" {
		fmt.Println("Local deployment — nothing to update.")
		return nil
	}

	fmt.Printf("Cloud provider %q update not yet implemented.\n", spec.Provider)
	fmt.Println("Use 'loka worker add' and 'loka worker remove' to manage workers.")
	return nil
}

func applyLocal(store *DeploymentStore, spec *DeploymentFile) error {
	lokad, err := findBinary("lokad")
	if err != nil {
		return err
	}

	store.Add(Deployment{
		Name:      spec.Name,
		Provider:  "local",
		Endpoint:  "http://localhost:6840",
		Workers:   1,
		Status:    "running",
		CreatedAt: time.Now(),
	})
	store.Active = spec.Name
	saveDeployments(store)

	fmt.Println("Starting lokad...")
	p := exec.Command(lokad)
	p.Env = os.Environ()
	if err := p.Start(); err != nil {
		return err
	}
	fmt.Printf("Server %q started (pid %d)\n", spec.Name, p.Process.Pid)
	fmt.Printf("  Endpoint: http://localhost:6840\n")
	return nil
}

func applyVM(store *DeploymentStore, spec *DeploymentFile) error {
	if spec.ControlPlane == nil || spec.ControlPlane.Address == "" {
		return fmt.Errorf("'controlplane.address' is required for vm provider")
	}

	sshUser, sshKey := resolveSSH(spec)
	cpAddr := spec.ControlPlane.Address
	cpPort := spec.ControlPlane.Port
	if cpPort == "" {
		cpPort = "6840"
	}

	// Step 1: Install and start control plane.
	fmt.Printf("[1/3] Setting up control plane on %s...\n", cpAddr)
	if err := sshRun(cpAddr, sshUser, sshKey, installScript()); err != nil {
		return fmt.Errorf("install failed on CP: %w", err)
	}
	if err := sshRun(cpAddr, sshUser, sshKey, "nohup lokad --role controlplane > /var/log/lokad.log 2>&1 &"); err != nil {
		return fmt.Errorf("failed to start lokad: %w", err)
	}
	fmt.Printf("  Control plane running at http://%s:%s\n\n", cpAddr, cpPort)

	// Step 2: Create worker token.
	fmt.Println("[2/3] Creating worker registration token...")
	tokenOut, err := sshOutput(cpAddr, sshUser, sshKey,
		fmt.Sprintf("sleep 2 && curl -s -X POST http://localhost:%s/api/v1/worker-tokens -H 'Content-Type: application/json' -d '{\"name\":\"%s\",\"expires_seconds\":0}'", cpPort, spec.Name))
	workerToken := ""
	if err == nil {
		workerToken = extractToken(tokenOut)
	}
	if workerToken == "" {
		fmt.Println("  Warning: could not create token automatically")
	}

	// Step 3: Set up workers.
	workerAddrs := make([]string, 0, len(spec.Workers))
	for i, w := range spec.Workers {
		if w.Address == "" {
			continue
		}
		fmt.Printf("[3/3] Setting up worker %d/%d (%s)...\n", i+1, len(spec.Workers), w.Address)
		if w.Address != cpAddr {
			if err := sshRun(w.Address, sshUser, sshKey, installScript()); err != nil {
				fmt.Printf("  Warning: install failed on %s: %v\n", w.Address, err)
				continue
			}
		}
		if err := startWorkerViaSSH(w.Address, sshUser, sshKey, cpAddr, workerToken, i+1, w.Labels); err != nil {
			fmt.Printf("  Warning: failed to start worker on %s: %v\n", w.Address, err)
			continue
		}
		fmt.Printf("  Worker running on %s\n", w.Address)
		workerAddrs = append(workerAddrs, w.Address)
	}

	// Save deployment.
	endpoint := fmt.Sprintf("http://%s:%s", cpAddr, cpPort)
	store.Add(Deployment{
		Name:      spec.Name,
		Provider:  "vm",
		Region:    cpAddr,
		Endpoint:  endpoint,
		Workers:   len(workerAddrs),
		Status:    "running",
		CreatedAt: time.Now(),
		Meta: map[string]string{
			"cp":           cpAddr,
			"workers":      strings.Join(workerAddrs, ","),
			"ssh_user":     sshUser,
			"ssh_key":      sshKey,
			"worker_token": workerToken,
		},
	})
	store.Active = spec.Name
	saveDeployments(store)

	fmt.Printf("\nServer %q ready (%d workers)\n", spec.Name, len(workerAddrs))
	return nil
}

func applyCloud(store *DeploymentStore, spec *DeploymentFile) error {
	opts := deployOpts{
		Name:     spec.Name,
		Provider: spec.Provider,
		Region:   spec.Region,
		Zone:     spec.Zone,
		Project:  spec.Project,
		Workers:  len(spec.Workers),
	}
	if opts.Workers == 0 {
		opts.Workers = 1
	}
	if spec.ControlPlane != nil {
		opts.InstanceType = spec.ControlPlane.InstanceType
	}

	var fn deployFunc
	switch spec.Provider {
	case "aws":
		fn = deployAWS
	case "gcp":
		fn = deployGCP
	case "azure":
		fn = deployAzure
	case "do":
		fn = deployDigitalOcean
	case "ovh":
		fn = deployOVH
	}

	if err := fn(opts); err != nil {
		return err
	}

	store.Add(Deployment{
		Name:      opts.Name,
		Provider:  opts.Provider,
		Region:    opts.Region,
		Endpoint:  "http://<pending>:6840",
		Workers:   opts.Workers,
		Status:    "provisioning",
		CreatedAt: time.Now(),
	})
	store.Active = opts.Name
	saveDeployments(store)
	fmt.Printf("\nServer %q created and set as active.\n", opts.Name)
	return nil
}

// reconcileVMWorkers compares desired workers with current and adds/removes as needed.
func reconcileVMWorkers(store *DeploymentStore, existing *Deployment, spec *DeploymentFile) error {
	sshUser, sshKey := resolveSSH(spec)

	// Current workers.
	currentWorkers := map[string]bool{}
	if w := existing.Meta["workers"]; w != "" {
		for _, addr := range strings.Split(w, ",") {
			addr = strings.TrimSpace(addr)
			if addr != "" {
				currentWorkers[addr] = true
			}
		}
	}

	// Desired workers.
	desiredWorkers := map[string]DeployWorkerSpec{}
	for _, w := range spec.Workers {
		if w.Address != "" {
			desiredWorkers[w.Address] = w
		}
	}

	// Diff.
	var toAdd []DeployWorkerSpec
	for addr, w := range desiredWorkers {
		if !currentWorkers[addr] {
			toAdd = append(toAdd, w)
		}
	}
	var toRemove []string
	for addr := range currentWorkers {
		if _, wanted := desiredWorkers[addr]; !wanted {
			toRemove = append(toRemove, addr)
		}
	}

	if len(toAdd) == 0 && len(toRemove) == 0 {
		fmt.Println("No changes — workers match the spec.")
		return nil
	}

	fmt.Printf("Changes: +%d workers, -%d workers\n\n", len(toAdd), len(toRemove))

	cpAddr := existing.Meta["cp"]
	workerToken := existing.Meta["worker_token"]

	// Remove workers.
	for _, addr := range toRemove {
		fmt.Printf("- Removing %s...\n", addr)
		stopCmd := "pkill -f loka-worker 2>/dev/null; systemctl stop loka-worker 2>/dev/null; echo done"
		if err := sshRun(addr, sshUser, sshKey, stopCmd); err != nil {
			fmt.Printf("  Warning: could not stop worker on %s: %v\n", addr, err)
		}
		delete(currentWorkers, addr)
	}

	// Add workers.
	workerNum := len(currentWorkers)
	for _, w := range toAdd {
		workerNum++
		fmt.Printf("+ Adding %s...\n", w.Address)
		if err := sshRun(w.Address, sshUser, sshKey, installScript()); err != nil {
			fmt.Printf("  Warning: install failed on %s: %v\n", w.Address, err)
			continue
		}
		if err := startWorkerViaSSH(w.Address, sshUser, sshKey, cpAddr, workerToken, workerNum, w.Labels); err != nil {
			fmt.Printf("  Warning: start failed on %s: %v\n", w.Address, err)
			continue
		}
		currentWorkers[w.Address] = true
	}

	// Update deployment.
	var addrs []string
	for addr := range currentWorkers {
		addrs = append(addrs, addr)
	}
	existing.Meta["workers"] = strings.Join(addrs, ",")
	existing.Workers = len(addrs)
	saveDeployments(store)

	fmt.Printf("\nServer %q updated (%d workers)\n", existing.Name, existing.Workers)
	return nil
}

// ── Helpers ─────────────────────────────────────────────

func resolveSSH(spec *DeploymentFile) (user, key string) {
	user = "root"
	if spec.SSH != nil {
		if spec.SSH.User != "" {
			user = spec.SSH.User
		}
		key = spec.SSH.Key
	}
	return
}

func startWorkerViaSSH(addr, sshUser, sshKey, cpAddr, workerToken string, num int, labels map[string]string) error {
	if labels == nil {
		labels = map[string]string{}
	}
	if _, ok := labels["node"]; !ok {
		labels["node"] = fmt.Sprintf("worker-%d", num)
	}

	var labelYAML strings.Builder
	for k, v := range labels {
		fmt.Fprintf(&labelYAML, "  %s: %q\n", k, v)
	}

	config := fmt.Sprintf(`sudo mkdir -p /etc/loka && cat > /etc/loka/worker.yaml << 'WCFG'
control_plane:
  address: "%s:6841"
data_dir: /var/loka/worker
provider: selfmanaged
token: "%s"
labels:
%sWCFG
nohup loka-worker --config /etc/loka/worker.yaml > /var/log/loka-worker.log 2>&1 &`, cpAddr, workerToken, labelYAML.String())

	return sshRun(addr, sshUser, sshKey, config)
}

func newDeployExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export [name] [file.yml]",
		Short: "Export a server config as YAML",
		Long: `Export the current deployment state as a YAML file that can be used with 'setup apply'.

Without arguments, prints the active server's config to stdout.
With a name, exports that server. With a file path, writes to file.

Examples:
  loka space export                    # Print active server YAML to stdout
  loka space export prod               # Print "prod" server YAML
  loka space export prod prod.yml      # Save to file
  loka space export > cluster.yml      # Redirect to file`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _ := loadDeployments()

			var d *Deployment
			if len(args) >= 1 {
				d = store.Get(args[0])
				if d == nil {
					return fmt.Errorf("server %q not found", args[0])
				}
			} else {
				d = store.GetActive()
				if d == nil {
					return fmt.Errorf("no active server")
				}
			}

			spec := deploymentToSpec(d)

			data, err := yaml.Marshal(spec)
			if err != nil {
				return fmt.Errorf("marshal YAML: %w", err)
			}

			if len(args) >= 2 {
				filePath := args[1]
				if err := os.WriteFile(filePath, data, 0o644); err != nil {
					return fmt.Errorf("write %s: %w", filePath, err)
				}
				fmt.Printf("Exported %q to %s\n", d.Name, filePath)
				return nil
			}

			fmt.Print(string(data))
			return nil
		},
	}
}

// deploymentToSpec converts a Deployment to a DeploymentFile for export.
func deploymentToSpec(d *Deployment) *DeploymentFile {
	spec := &DeploymentFile{
		Name:     d.Name,
		Provider: d.Provider,
	}

	if d.Region != "" && d.Provider != "vm" && d.Provider != "local" {
		spec.Region = d.Region
	}

	// SSH config from metadata.
	sshUser := d.Meta["ssh_user"]
	sshKey := d.Meta["ssh_key"]
	if sshUser != "" || sshKey != "" {
		spec.SSH = &DeploySSH{User: sshUser, Key: sshKey}
	}

	// Control plane.
	cpAddr := d.Meta["cp"]
	if cpAddr != "" {
		spec.ControlPlane = &DeployCPSpec{Address: cpAddr}
	}

	// Workers from metadata.
	if workerList := d.Meta["workers"]; workerList != "" {
		for _, addr := range strings.Split(workerList, ",") {
			addr = strings.TrimSpace(addr)
			if addr != "" {
				spec.Workers = append(spec.Workers, DeployWorkerSpec{Address: addr})
			}
		}
	}

	// For cloud providers without explicit worker addresses, generate count-based entries.
	if len(spec.Workers) == 0 && d.Workers > 0 && d.Provider != "local" {
		for i := 0; i < d.Workers; i++ {
			spec.Workers = append(spec.Workers, DeployWorkerSpec{})
		}
	}

	return spec
}
