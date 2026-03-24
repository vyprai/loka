package main

import (
	"context"
	crypto_tls "crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	"github.com/vyprai/loka/internal/objstore"
	azureobjstore "github.com/vyprai/loka/internal/objstore/azure"
	gcsobjstore "github.com/vyprai/loka/internal/objstore/gcs"
	leaderobjstore "github.com/vyprai/loka/internal/objstore/leader"
	localobjstore "github.com/vyprai/loka/internal/objstore/local"
	s3objstore "github.com/vyprai/loka/internal/objstore/s3"
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
	replicatedstore "github.com/vyprai/loka/internal/store/replicated"
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

	// NOTE: In HA mode with SQLite/local objstore, the store and objstore are
	// wrapped after coordinator init to enable Raft-based replication.
	// See "HA replication" section below.

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
	var objStore objstore.ObjectStore
	switch cfg.ObjectStore.Type {
	case "local":
		s, err := localobjstore.New(cfg.ObjectStore.Path)
		if err != nil {
			logger.Error("failed to create local object store", "error", err)
			os.Exit(1)
		}
		objStore = s
	case "s3", "minio":
		s, err := s3objstore.New(ctx, s3objstore.Config{
			Region:   cfg.ObjectStore.Region,
			Endpoint: cfg.ObjectStore.Endpoint,
		})
		if err != nil {
			logger.Error("failed to create S3 object store", "error", err)
			os.Exit(1)
		}
		objStore = s
	case "gcs":
		s, err := gcsobjstore.New(ctx)
		if err != nil {
			logger.Error("failed to create GCS object store", "error", err)
			os.Exit(1)
		}
		defer s.Close()
		objStore = s
	case "azure":
		s, err := azureobjstore.New(ctx, azureobjstore.Config{
			Account: cfg.ObjectStore.Account,
		})
		if err != nil {
			logger.Error("failed to create Azure object store", "error", err)
			os.Exit(1)
		}
		objStore = s
	default:
		logger.Error("unsupported object store type", "type", cfg.ObjectStore.Type)
		os.Exit(1)
	}
	logger.Info("object store ready", "type", cfg.ObjectStore.Type)

	// ── HA replication ──────────────────────────────────────
	// In HA mode, wrap the store and object store for cross-node sync.
	if cfg.Mode == "ha" {
		// Replicate SQLite writes through Raft consensus.
		if cfg.Database.Driver == "sqlite" {
			db = replicatedstore.New(db, coordinator, logger)
			logger.Info("HA SQLite replication enabled")
		}

		// Forward objstore writes to the leader.
		scheme := "https"
		if cfg.TLS.AutoTLS != nil && !*cfg.TLS.AutoTLS && cfg.TLS.CertFile == "" {
			scheme = "http"
		}
		apiPort := cfg.ListenAddr
		if len(apiPort) > 0 && apiPort[0] == ':' {
			apiPort = apiPort[1:]
		}
		objStore = leaderobjstore.New(leaderobjstore.Config{
			Local:   objStore,
			Leader:  coordinator,
			Name:    "control-plane",
			Scheme:  scheme,
			APIPort: apiPort,
			Token:   cfg.Auth.APIKey,
		})
		logger.Info("HA object store proxy enabled")
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
	imgMgr := image.NewManager(objStore, cfg.DataDir, logger)

	// Initialize worker registry.
	registry := worker.NewRegistry(db, logger)

	// Initialize scheduler.
	sched := scheduler.New(registry, scheduler.Strategy(cfg.Scheduler.Strategy))

	// Initialize session manager.
	sm := session.NewManager(db, registry, sched, imgMgr, objStore, logger)

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
			KernelPath: envOrDefault("LOKA_KERNEL_PATH", cfg.DataDir+"/kernel/vmlinux"),
			RootfsPath: envOrDefault("LOKA_ROOTFS_PATH", cfg.DataDir+"/rootfs/rootfs.ext4"),
			DataDir:    cfg.DataDir + "/worker-data",
		}
		dataDir := cfg.DataDir + "/worker-data"
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
		// Auto-TLS: try loading shared certs from objstore, generate if not found.
		tlsDir := cfg.DataDir + "/tls"
		if err := os.MkdirAll(tlsDir, 0700); err != nil {
			logger.Error("failed to create TLS dir", "error", err)
			os.Exit(1)
		}

		tlsFiles := []string{"ca.crt", "ca.key", "server.crt", "server.key"}
		certsFromObjStore := true

		// Try to load existing certs from objstore (shared across HA nodes).
		caReader, objErr := objStore.Get(ctx, "loka", "tls/ca.crt")
		if objErr == nil {
			caReader.Close()
			// All TLS files should exist — download them.
			for _, name := range tlsFiles {
				reader, err := objStore.Get(ctx, "loka", "tls/"+name)
				if err != nil {
					logger.Warn("TLS file missing from objstore, will regenerate", "file", name, "error", err)
					certsFromObjStore = false
					break
				}
				localPath := filepath.Join(tlsDir, name)
				data, err := io.ReadAll(reader)
				reader.Close()
				if err != nil {
					logger.Warn("failed to read TLS file from objstore", "file", name, "error", err)
					certsFromObjStore = false
					break
				}
				if err := os.WriteFile(localPath, data, 0600); err != nil {
					logger.Error("failed to write TLS file locally", "file", localPath, "error", err)
					os.Exit(1)
				}
				logger.Debug("loaded TLS file from objstore", "file", name)
			}
		} else {
			certsFromObjStore = false
		}

		if certsFromObjStore {
			logger.Info("TLS certs loaded from object store")
		} else {
			// Generate new certs locally.
			_, _, err := tlsutil.GenerateCA(tlsDir)
			if err != nil {
				logger.Error("failed to generate CA", "error", err)
				os.Exit(1)
			}
			_, _, err = tlsutil.GenerateServerCert(filepath.Join(tlsDir, "ca.crt"), filepath.Join(tlsDir, "ca.key"), tlsDir, cfg.TLS.SANs)
			if err != nil {
				logger.Error("failed to generate server cert", "error", err)
				os.Exit(1)
			}

			// Upload generated certs to objstore for other HA nodes.
			for _, name := range tlsFiles {
				localPath := filepath.Join(tlsDir, name)
				f, err := os.Open(localPath)
				if err != nil {
					logger.Warn("failed to open TLS file for upload", "file", localPath, "error", err)
					continue
				}
				info, err := f.Stat()
				if err != nil {
					f.Close()
					logger.Warn("failed to stat TLS file for upload", "file", localPath, "error", err)
					continue
				}
				if err := objStore.Put(ctx, "loka", "tls/"+name, f, info.Size()); err != nil {
					logger.Warn("failed to upload TLS file to objstore", "file", name, "error", err)
				} else {
					logger.Debug("uploaded TLS file to objstore", "file", name)
				}
				f.Close()
			}
			logger.Info("TLS certs generated and uploaded to object store")
		}

		caPath := filepath.Join(tlsDir, "ca.crt")
		certPath := filepath.Join(tlsDir, "server.crt")
		keyPath := filepath.Join(tlsDir, "server.key")
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
		ObjStore:   objStore,
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
