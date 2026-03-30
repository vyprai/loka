package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vyprai/loka/internal/config"
	"github.com/vyprai/loka/internal/loka"
	proxyobjstore "github.com/vyprai/loka/internal/objstore/proxy"
	"github.com/vyprai/loka/internal/worker"
	"github.com/vyprai/loka/pkg/lokavm"
	"github.com/vyprai/loka/pkg/version"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	logger.Info("starting loka-worker", "version", version.Version)

	// Load config.
	var cfg config.WorkerConfig
	if configPath := os.Getenv("LOKA_WORKER_CONFIG"); configPath != "" {
		if err := config.Load(configPath, &cfg); err != nil {
			logger.Error("failed to load config", "error", err)
			os.Exit(1)
		}
	}
	cfg.Defaults()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create proxy object store — all writes go through the control plane.
	scheme := "https"
	if cfg.ControlPlane.Insecure && !cfg.ControlPlane.TLS {
		scheme = "http"
	}
	cpURL := fmt.Sprintf("%s://%s", scheme, cfg.ControlPlane.Address)
	objStore := proxyobjstore.New(proxyobjstore.Config{
		BaseURL:  cpURL,
		Token:    cfg.Token,
		Insecure: cfg.ControlPlane.Insecure,
	})

	// Create lokavm hypervisor.
	hvConfig := lokavm.HypervisorConfig{
		KernelPath: envOrDefault("LOKA_KERNEL_PATH", cfg.DataDir+"/kernel/vmlinux"),
		DataDir:    cfg.DataDir,
	}
	// Try hypervisors in order: VZ (macOS) → Cloud Hypervisor (Linux) → KVM (Linux).
	var hypervisor lokavm.Hypervisor
	if vz, err := lokavm.NewHypervisor(hvConfig, logger); err == nil {
		hypervisor = vz
		logger.Info("using Apple VZ hypervisor")
	} else if ch, err := lokavm.NewCHHypervisor(hvConfig, logger); err == nil {
		hypervisor = ch
		logger.Info("using Cloud Hypervisor")
	} else if kvm, err := lokavm.NewKVMHypervisor(hvConfig, logger); err == nil {
		hypervisor = kvm
		logger.Info("using KVM hypervisor")
	} else {
		logger.Error("no hypervisor available")
		os.Exit(1)
	}
	agent, err := worker.NewAgent(cfg.Provider, cfg.Labels, cfg.DataDir, objStore, hypervisor, logger)
	if err != nil {
		logger.Error("failed to create agent", "error", err)
		os.Exit(1)
	}
	agent.SetRemoteMode(true) // loka-worker is always a remote worker.

	// Create CP client.
	var tlsOpts *worker.CPClientTLS
	if scheme == "https" {
		tlsOpts = &worker.CPClientTLS{
			CACertPath: cfg.ControlPlane.CACert,
			Insecure:   cfg.ControlPlane.Insecure,
		}
	}
	cpClient := worker.NewCPClient(cpURL, cfg.Token, tlsOpts, logger)

	// Register with control plane (retry with exponential backoff).
	logger.Info("registering with control plane", "address", cfg.ControlPlane.Address)
	var workerID string
	{
		backoff := 1 * time.Second
		maxBackoff := 30 * time.Second
		for {
			var regErr error
			workerID, regErr = cpClient.Register(ctx, agent.Hostname(), agent.Provider(), agent.Capacity(), agent.Labels())
			if regErr == nil {
				break
			}
			logger.Warn("registration failed, retrying", "error", regErr, "backoff", backoff)
			select {
			case <-ctx.Done():
				logger.Error("registration cancelled")
				os.Exit(1)
			case <-time.After(backoff):
			}
			backoff = time.Duration(float64(backoff) * 2)
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
	agent.SetID(workerID)
	logger.Info("registered", "worker_id", workerID)

	// Start command polling loop.
	// In production, this is a gRPC bidirectional stream.
	// For MVP, the worker polls for commands via the session manager.
	// Since we don't have a poll endpoint yet, the worker just sends heartbeats
	// and waits for the CP to push commands (which happens in-process in dev mode).

	// Graceful shutdown.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("shutting down worker...")
		cancel()
	}()

	// Heartbeat loop.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	logger.Info("worker running, sending heartbeats")
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			logger.Info("worker stopped")
			return
		case <-ticker.C:
			hb := agent.Heartbeat()
			status, err := cpClient.SendHeartbeat(ctx, *hb)
			if err != nil {
				consecutiveFailures++
				logger.Warn("heartbeat failed", "error", err, "failures", consecutiveFailures)
				continue
			}
			consecutiveFailures = 0

			// If CP doesn't recognize us, re-register.
			if status == "unknown_worker" {
				logger.Warn("CP returned unknown_worker, re-registering")
				newID, regErr := cpClient.Register(ctx, agent.Hostname(), agent.Provider(), agent.Capacity(), agent.Labels())
				if regErr != nil {
					logger.Error("re-registration failed", "error", regErr)
				} else {
					agent.SetID(newID)
					workerID = newID
					logger.Info("re-registered", "worker_id", newID)
				}
			}
		}
	}
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// Ensure loka import is used.
var _ loka.ExecMode
