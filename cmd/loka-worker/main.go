package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rizqme/loka/internal/config"
	"github.com/rizqme/loka/internal/loka"
	localobjstore "github.com/rizqme/loka/internal/objstore/local"
	"github.com/rizqme/loka/internal/worker"
	"github.com/rizqme/loka/internal/worker/vm"
	"github.com/rizqme/loka/pkg/version"
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

	// Create local object store for the worker.
	objStore, err := localobjstore.New(cfg.DataDir + "/objstore")
	if err != nil {
		logger.Error("failed to create object store", "error", err)
		os.Exit(1)
	}

	// Create worker agent with Firecracker config.
	fcConfig := vm.FirecrackerConfig{
		BinaryPath: envOrDefault("LOKA_FIRECRACKER_BIN", "/usr/local/bin/firecracker"),
		KernelPath: envOrDefault("LOKA_KERNEL_PATH", cfg.DataDir+"/kernel/vmlinux"),
		RootfsPath: envOrDefault("LOKA_ROOTFS_PATH", cfg.DataDir+"/rootfs/rootfs.ext4"),
		DataDir:    cfg.DataDir,
	}
	agent, err := worker.NewAgent(cfg.Provider, cfg.Labels, cfg.DataDir, objStore, fcConfig, logger)
	if err != nil {
		logger.Error("failed to create agent", "error", err)
		os.Exit(1)
	}

	// Create CP client.
	scheme := "https"
	if cfg.ControlPlane.Insecure && !cfg.ControlPlane.TLS {
		scheme = "http"
	}
	cpURL := fmt.Sprintf("%s://%s", scheme, cfg.ControlPlane.Address)
	var tlsOpts *worker.CPClientTLS
	if scheme == "https" {
		tlsOpts = &worker.CPClientTLS{
			CACertPath: cfg.ControlPlane.CACert,
			Insecure:   cfg.ControlPlane.Insecure,
		}
	}
	cpClient := worker.NewCPClient(cpURL, cfg.Token, tlsOpts, logger)

	// Register with control plane.
	logger.Info("registering with control plane", "address", cfg.ControlPlane.Address)
	workerID, err := cpClient.Register(ctx, agent.Hostname(), agent.Provider(), agent.Capacity(), agent.Labels())
	if err != nil {
		logger.Error("failed to register with control plane", "error", err)
		os.Exit(1)
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
	for {
		select {
		case <-ctx.Done():
			logger.Info("worker stopped")
			return
		case <-ticker.C:
			hb := agent.Heartbeat()
			logger.Debug("heartbeat", "sessions", hb.SessionCount, "status", hb.Status)
			// In production, send heartbeat over gRPC stream.
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
