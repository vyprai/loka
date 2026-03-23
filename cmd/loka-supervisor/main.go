package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/supervisor"
	"github.com/vyprai/loka/pkg/version"
)

// loka-supervisor runs inside the Firecracker microVM as the init process (PID 1).
// It listens on vsock and handles all command execution, enforcing the exec policy
// and sandbox restrictions.
//
// Boot sequence:
//   1. Firecracker starts the VM with: init=/usr/local/bin/loka-supervisor
//   2. Supervisor initializes the sandbox (mount RO/RW, seccomp, PATH restriction)
//   3. Supervisor listens on vsock port 52
//   4. Worker (host) connects via vsock and sends exec/approve/deny commands
func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("loka-supervisor starting", "version", version.Version)

	// Default policy — will be updated by the worker via set_policy RPC.
	policy := loka.DefaultExecPolicy()
	mode := loka.ModeExplore

	// Check environment for initial mode.
	if m := os.Getenv("LOKA_MODE"); m != "" {
		mode = loka.ExecMode(m)
	}

	// Determine listen address.
	// In production: vsock listener on port 52.
	// For local testing: unix domain socket.
	listenAddr := os.Getenv("LOKA_SUPERVISOR_SOCK")
	if listenAddr == "" {
		listenAddr = "/tmp/loka-supervisor.sock"
	}

	server := supervisor.NewServer(policy, mode, logger)

	// Graceful shutdown.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("supervisor shutting down")
		server.Stop()
	}()

	if err := server.ListenAndServe(listenAddr); err != nil {
		logger.Error("supervisor error", "error", err)
		os.Exit(1)
	}
}
