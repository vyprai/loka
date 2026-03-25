package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

func newDNSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "Manage DNS resolution for .loka domains",
		Long: `Configure local DNS so that *.loka resolves to 127.0.0.1.

  loka dns enable    # Set up resolver and start DNS server
  loka dns disable   # Remove resolver config and stop DNS server
  loka dns status    # Show current DNS status`,
	}
	cmd.AddCommand(
		newDNSEnableCmd(),
		newDNSDisableCmd(),
		newDNSStatusCmd(),
	)
	return cmd
}

func newDNSEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Enable DNS resolution for .loka domains",
		RunE: func(cmd *cobra.Command, args []string) error {
			// 1. Create the OS resolver config.
			if err := createResolverConfig(); err != nil {
				return err
			}

			// 2. Enable domain proxy + DNS on the server.
			client := newClient()
			var resp map[string]any
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/admin/dns", map[string]any{"enabled": true}, &resp); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not enable DNS on server: %v\n", err)
				fmt.Fprintf(os.Stderr, "The resolver is configured but the DNS server may not be running.\n")
				fmt.Fprintf(os.Stderr, "Ensure lokad is running with domain.dns_enabled: true in the config.\n")
			} else {
				fmt.Println("DNS server enabled on lokad.")
			}

			fmt.Println("DNS resolution for .loka domains is now enabled.")
			fmt.Println("  *.loka -> 127.0.0.1 (via port 5453)")
			fmt.Println()
			fmt.Println("Test it:")
			fmt.Println("  dig @127.0.0.1 -p 5453 test.loka")
			fmt.Println("  curl http://my-app.loka:6843")
			return nil
		},
	}
}

func newDNSDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Disable DNS resolution for .loka domains",
		RunE: func(cmd *cobra.Command, args []string) error {
			// 1. Remove the OS resolver config.
			if err := removeResolverConfig(); err != nil {
				return err
			}

			// 2. Disable DNS on the server.
			client := newClient()
			var resp map[string]any
			if err := client.Raw(cmd.Context(), "POST", "/api/v1/admin/dns", map[string]any{"enabled": false}, &resp); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not disable DNS on server: %v\n", err)
			} else {
				fmt.Println("DNS server disabled on lokad.")
			}

			fmt.Println("DNS resolution for .loka domains has been disabled.")
			return nil
		},
	}
}

func newDNSStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show DNS resolution status for .loka domains",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Check resolver file.
			resolverPath := resolverFilePath()
			resolverExists := false
			if _, err := os.Stat(resolverPath); err == nil {
				resolverExists = true
			}

			fmt.Printf("Resolver file:  ")
			if resolverExists {
				fmt.Printf("%s (exists)\n", resolverPath)
			} else {
				fmt.Printf("%s (not found)\n", resolverPath)
			}

			// Check DNS server.
			fmt.Printf("DNS server:     ")
			out, err := exec.Command("dig", "@127.0.0.1", "-p", "5453", "test.loka", "+short", "+time=2", "+tries=1").Output()
			if err != nil {
				fmt.Println("not responding")
			} else {
				answer := strings.TrimSpace(string(out))
				if answer == "" {
					fmt.Println("responding but no answer")
				} else {
					fmt.Printf("responding (%s)\n", answer)
				}
			}

			// Overall status.
			fmt.Printf("Status:         ")
			if resolverExists {
				fmt.Println("enabled")
			} else {
				fmt.Println("disabled")
			}

			return nil
		},
	}
}

// resolverFilePath returns the path to the OS-level resolver configuration.
func resolverFilePath() string {
	if runtime.GOOS == "linux" {
		// Check for systemd-resolved first.
		if _, err := os.Stat("/etc/systemd/resolved.conf.d"); err == nil {
			return "/etc/systemd/resolved.conf.d/loka.conf"
		}
	}
	// macOS uses /etc/resolver/<domain>, Linux fallback does the same.
	return "/etc/resolver/loka"
}

// createResolverConfig creates the OS-level DNS resolver config.
func createResolverConfig() error {
	path := resolverFilePath()

	if runtime.GOOS == "linux" && strings.Contains(path, "systemd") {
		// systemd-resolved config.
		content := "[Resolve]\nDNS=127.0.0.1\nDomains=~loka\n"
		return writeSudoFile(path, content)
	}

	// macOS /etc/resolver/loka or Linux fallback.
	content := "nameserver 127.0.0.1\nport 5453\n"

	// Ensure /etc/resolver directory exists.
	dir := "/etc/resolver"
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		fmt.Printf("Creating %s (requires sudo)...\n", dir)
		if err := exec.Command("sudo", "mkdir", "-p", dir).Run(); err != nil {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}

	return writeSudoFile(path, content)
}

// removeResolverConfig removes the OS-level DNS resolver config.
func removeResolverConfig() error {
	path := resolverFilePath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // Already removed.
	}
	fmt.Printf("Removing %s (requires sudo)...\n", path)
	if err := exec.Command("sudo", "rm", "-f", path).Run(); err != nil {
		return fmt.Errorf("failed to remove %s: %w", path, err)
	}

	// If systemd-resolved, restart it.
	if runtime.GOOS == "linux" && strings.Contains(path, "systemd") {
		exec.Command("sudo", "systemctl", "restart", "systemd-resolved").Run()
	}
	return nil
}

// writeSudoFile writes content to path using sudo tee.
func writeSudoFile(path, content string) error {
	fmt.Printf("Writing %s (requires sudo)...\n", path)
	cmd := exec.Command("sudo", "tee", path)
	cmd.Stdin = strings.NewReader(content)
	cmd.Stdout = nil // suppress tee's stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}
