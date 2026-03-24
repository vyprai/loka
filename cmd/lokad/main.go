package main

import (
	"context"
	crypto_tls "crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	"github.com/vyprai/loka/internal/config"
	"github.com/vyprai/loka/internal/controlplane"
	"github.com/vyprai/loka/internal/controlplane/api"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/ha"
	"github.com/vyprai/loka/internal/controlplane/scheduler"
	"github.com/vyprai/loka/internal/controlplane/session"
	"github.com/vyprai/loka/internal/controlplane/worker"
	localobjstore "github.com/vyprai/loka/internal/objstore/local"
	"github.com/vyprai/loka/internal/worker/vm"
	"github.com/vyprai/loka/internal/provider"
	"github.com/vyprai/loka/pkg/tlsutil"

	"google.golang.org/grpc/credentials"
	provaws "github.com/vyprai/loka/internal/provider/aws"
	provazure "github.com/vyprai/loka/internal/provider/azure"
	provdo "github.com/vyprai/loka/internal/provider/digitalocean"
	provgcp "github.com/vyprai/loka/internal/provider/gcp"
	provlocal "github.com/vyprai/loka/internal/provider/local"
	provovh "github.com/vyprai/loka/internal/provider/ovh"
	provsm "github.com/vyprai/loka/internal/provider/selfmanaged"
	"github.com/vyprai/loka/internal/store"
	"github.com/vyprai/loka/pkg/version"

	// Register store drivers.
	_ "github.com/vyprai/loka/internal/store/postgres"
	_ "github.com/vyprai/loka/internal/store/sqlite"

	// Raft coordinator registers via init() in ha/raft.go — no import needed.
)

func main() {
	// Parse CLI flags.
	var (
		configPath string
		role       string
	)
	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--config", "-c":
			if i+1 < len(os.Args) {
				configPath = os.Args[i+1]
				i++
			}
		case "--role":
			if i+1 < len(os.Args) {
				role = os.Args[i+1]
				i++
			}
		}
	}

	// Load config.
	var cfg config.ControlPlaneConfig
	if configPath == "" {
		configPath = os.Getenv("LOKA_CONFIG")
	}
	if configPath != "" {
		if err := config.Load(configPath, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
			os.Exit(1)
		}
	}
	// CLI flag overrides config file.
	if role != "" {
		cfg.Role = role
	}
	cfg.Defaults()

	// Configure logging.
	logLevel := slog.LevelInfo
	switch cfg.Logging.Level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	var logHandler slog.Handler
	if cfg.Logging.Format == "json" {
		logHandler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	} else {
		logHandler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	}
	logger := slog.New(logHandler)
	slog.SetDefault(logger)

	logger.Info("starting lokad", "version", version.Version, "commit", version.Commit, "role", cfg.Role)

	// Initialize store via factory.
	db, err := store.Open(store.Config{
		Driver: cfg.Database.Driver,
		DSN:    cfg.Database.DSN,
	})
	if err != nil {
		logger.Error("failed to open database", "driver", cfg.Database.Driver, "error", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := db.Migrate(ctx); err != nil {
		logger.Error("failed to migrate database", "error", err)
		os.Exit(1)
	}

	logger.Info("database ready", "driver", cfg.Database.Driver)

	// Initialize coordinator via factory.
	coordinator, err := ha.Open(ha.Config{
		Type:      cfg.Coordinator.Type,
		Address:   cfg.Coordinator.Address,
		NodeID:    cfg.Coordinator.NodeID,
		DataDir:   cfg.Coordinator.DataDir,
		Bootstrap: cfg.Coordinator.Bootstrap,
		Peers:     cfg.Coordinator.Peers,
	})
	if err != nil {
		logger.Error("failed to create coordinator", "type", cfg.Coordinator.Type, "error", err)
		os.Exit(1)
	}
	defer coordinator.Close()

	logger.Info("coordinator ready", "type", cfg.Coordinator.Type)

	// Initialize object store.
	objStore, err := localobjstore.New(cfg.ObjectStore.Path)
	if err != nil {
		logger.Error("failed to create object store", "error", err)
		os.Exit(1)
	}

	// Initialize provider registry.
	providerRegistry := provider.NewRegistry()
	providerRegistry.Register(provlocal.New())
	providerRegistry.Register(provsm.New(db))
	providerRegistry.Register(provaws.New(provaws.Config{}, logger))
	providerRegistry.Register(provgcp.New(provgcp.Config{}, logger))
	providerRegistry.Register(provazure.New(provazure.Config{}, logger))
	providerRegistry.Register(provovh.New(provovh.Config{}, logger))
	providerRegistry.Register(provdo.New(provdo.Config{}, logger))
	logger.Info("providers registered", "count", len(providerRegistry.List()))

	// Initialize image manager (Docker images → Firecracker rootfs).
	imgMgr := image.NewManager(objStore, cfg.ObjectStore.Path, logger)

	// Initialize worker registry.
	registry := worker.NewRegistry(db, logger)

	// Initialize scheduler.
	sched := scheduler.New(registry, scheduler.Strategy(cfg.Scheduler.Strategy))

	// Initialize session manager.
	sm := session.NewManager(db, registry, sched, imgMgr, logger)

	// Initialize drainer with migration callback.
	drainer := worker.NewDrainer(registry, db, sm.MigrateSession, logger)

	// Start worker health monitor (only on leader in HA mode).
	monitor := worker.NewMonitor(registry, db, sm.MigrateSession, worker.DefaultMonitorConfig(), logger)

	if cfg.Mode == "ha" {
		// In HA mode, only the leader runs the monitor.
		go coordinator.ElectLeader(ctx, "control-plane", func(leaderCtx context.Context) {
			logger.Info("this instance is the leader")
			monitor.Start(leaderCtx)
		})
	} else {
		// Single mode — always run the monitor.
		go monitor.Start(ctx)
	}

	// Start embedded local worker unless running as control plane only.
	if cfg.Role != "controlplane" {
		fcConfig := vm.FirecrackerConfig{
			BinaryPath: envOrDefault("LOKA_FIRECRACKER_BIN", "/usr/local/bin/firecracker"),
			KernelPath: envOrDefault("LOKA_KERNEL_PATH", cfg.ObjectStore.Path+"/kernel/vmlinux"),
			RootfsPath: envOrDefault("LOKA_ROOTFS_PATH", cfg.ObjectStore.Path+"/rootfs/rootfs.ext4"),
			DataDir:    cfg.ObjectStore.Path + "/worker-data",
		}
		dataDir := cfg.ObjectStore.Path + "/worker-data"
		localWorker, err := controlplane.NewLocalWorker(registry, sm, objStore, dataDir, fcConfig, logger)
		if err != nil {
			logger.Error("failed to create local worker", "error", err)
			os.Exit(1)
		}
		localWorker.Start(ctx)
		logger.Info("embedded worker started")
	} else {
		logger.Info("running as control plane only (no embedded worker)")
	}

	// ── TLS initialization ──────────────────────────────────
	autoTLS := cfg.TLS.AutoTLS == nil || *cfg.TLS.AutoTLS // default: true
	var serverTLSCfg *crypto_tls.Config
	var caCertPath string

	if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
		// User-provided certs.
		serverTLSCfg, err = tlsutil.LoadServerTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.CACertFile)
		if err != nil {
			logger.Error("failed to load TLS certs", "error", err)
			os.Exit(1)
		}
		caCertPath = cfg.TLS.CACertFile
		logger.Info("TLS enabled (user-provided certs)", "cert", cfg.TLS.CertFile)
	} else if autoTLS {
		// Auto-TLS: generate CA + server cert.
		tlsDir := cfg.ObjectStore.Path + "/tls"
		caPath, _, err := tlsutil.GenerateCA(tlsDir)
		if err != nil {
			logger.Error("failed to generate CA", "error", err)
			os.Exit(1)
		}
		certPath, keyPath, err := tlsutil.GenerateServerCert(caPath, tlsDir+"/ca.key", tlsDir, cfg.TLS.SANs)
		if err != nil {
			logger.Error("failed to generate server cert", "error", err)
			os.Exit(1)
		}
		serverTLSCfg, err = tlsutil.LoadServerTLS(certPath, keyPath, caPath)
		if err != nil {
			logger.Error("failed to load auto-TLS config", "error", err)
			os.Exit(1)
		}
		caCertPath = caPath
		logger.Info("auto-TLS enabled", "ca_cert", caPath, "server_cert", certPath)
	} else {
		// Plaintext mode.
		logger.Warn("════════════════════════════════════════")
		logger.Warn("  WARNING: TLS is DISABLED")
		logger.Warn("  All connections are UNENCRYPTED")
		logger.Warn("  Set tls.auto: true or provide certs")
		logger.Warn("════════════════════════════════════════")
		if cfg.Mode == "ha" && !cfg.TLS.AllowInsecure {
			logger.Error("HA mode requires TLS. Set tls.allow_insecure: true to override (NOT RECOMMENDED)")
			os.Exit(1)
		}
	}

	// Pass TLS config to Raft coordinator if using raft.
	if serverTLSCfg != nil && cfg.Coordinator.Type == "raft" {
		// Re-open coordinator with TLS. The initial open was without TLS.
		coordinator.Close()
		coordinator, err = ha.Open(ha.Config{
			Type:      cfg.Coordinator.Type,
			Address:   cfg.Coordinator.Address,
			NodeID:    cfg.Coordinator.NodeID,
			DataDir:   cfg.Coordinator.DataDir,
			Bootstrap: cfg.Coordinator.Bootstrap,
			Peers:     cfg.Coordinator.Peers,
			TLSConfig: serverTLSCfg,
		})
		if err != nil {
			logger.Error("failed to create coordinator with TLS", "error", err)
			os.Exit(1)
		}
		logger.Info("raft coordinator restarted with TLS")
	}

	// ── Initialize API server ───────────────────────────────
	srv := api.NewServer(sm, registry, providerRegistry, imgMgr, drainer, db, logger, api.ServerOpts{
		APIKey:     cfg.Auth.APIKey,
		Retention:  cfg.Retention,
		CACertPath: caCertPath,
	})

	httpServer := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: srv.Handler(),
	}

	// ── Start gRPC server ───────────────────────────────────
	var grpcOpts []grpc.ServerOption
	if serverTLSCfg != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(serverTLSCfg)))
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	grpcAPI := api.NewGRPCServer(sm, registry, logger)
	grpcAPI.Register(grpcSrv)

	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		logger.Error("failed to listen on gRPC addr", "addr", cfg.GRPCAddr, "error", err)
		os.Exit(1)
	}
	go func() {
		logger.Info("gRPC server listening", "addr", cfg.GRPCAddr, "tls", serverTLSCfg != nil)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			logger.Error("gRPC server error", "error", err)
		}
	}()

	// ── Graceful shutdown ───────────────────────────────────
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("shutting down...")
		cancel()
		grpcSrv.GracefulStop()
		httpServer.Shutdown(context.Background())
	}()

	// ── Start HTTP server ───────────────────────────────────
	if serverTLSCfg != nil {
		httpServer.TLSConfig = serverTLSCfg
		logger.Info(fmt.Sprintf("listening on %s (TLS, mode=%s, db=%s, coordinator=%s)",
			cfg.ListenAddr, cfg.Mode, cfg.Database.Driver, cfg.Coordinator.Type))
		if err := httpServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	} else {
		logger.Info(fmt.Sprintf("listening on %s (mode=%s, db=%s, coordinator=%s)",
			cfg.ListenAddr, cfg.Mode, cfg.Database.Driver, cfg.Coordinator.Type))
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}

	_ = caCertPath // Will be used for CA cert distribution endpoint
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
