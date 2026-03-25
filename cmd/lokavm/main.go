//go:build darwin

package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	lokadns "github.com/vyprai/loka/internal/dns"
)

func main() {
	var (
		dataDir  = flag.String("data-dir", defaultDataDir(), "Data directory")
		cpus     = flag.Int("cpus", 4, "VM CPU cores")
		memory   = flag.Int("memory", 8192, "VM memory in MB")
		listen   = flag.String("listen", ":6840", "API listen address")
		grpcAddr = flag.String("grpc", ":6841", "gRPC listen address")
		dnsAddr  = flag.String("dns", ":5453", "DNS server listen address")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	logger.Info("lokavm starting",
		"data_dir", *dataDir,
		"cpus", *cpus,
		"memory_mb", *memory,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Ensure kernel + initrd + rootfs assets exist in dataDir.
	kernel, initrd, rootfs, err := ensureAssets(*dataDir)
	if err != nil {
		logger.Error("failed to ensure assets", "error", err)
		os.Exit(1)
	}

	// 2. Boot VZ VM.
	vm, err := bootVM(ctx, kernel, initrd, rootfs, *cpus, *memory, *dataDir, logger)
	if err != nil {
		logger.Error("failed to boot VM", "error", err)
		os.Exit(1)
	}
	defer vm.Stop()

	// 3. Wait for lokad to become ready inside the VM.
	logger.Info("waiting for lokad inside VM...")
	if err := waitForLokad(vm, 60*time.Second); err != nil {
		logger.Error("lokad not ready", "error", err)
		os.Exit(1)
	}
	logger.Info("lokad ready")

	// 4. Start reverse proxy: host ports -> VM guest (NAT IP) -> lokad.
	proxy := NewVsockProxy(vm, logger)

	// HTTP/HTTPS API proxy.
	httpServer := &http.Server{Addr: *listen, Handler: proxy}
	go func() {
		logger.Info("API proxy listening", "addr", *listen)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server error", "error", err)
		}
	}()

	// gRPC proxy (raw TCP relay).
	go func() {
		logger.Info("gRPC proxy listening", "addr", *grpcAddr)
		startTCPProxy(*grpcAddr, vm, 6841, logger)
	}()

	// 5. Start DNS server on host so *.loka resolves to 127.0.0.1.
	dnsServer := lokadns.NewServer("loka", "127.0.0.1", *dnsAddr)
	if err := dnsServer.Start(); err != nil {
		logger.Warn("DNS server failed to start", "addr", *dnsAddr, "error", err)
	} else {
		logger.Info("DNS server listening", "addr", *dnsAddr, "domain", "loka")
		defer dnsServer.Stop()
	}

	logger.Info("lokavm ready",
		"api", *listen,
		"grpc", *grpcAddr,
		"dns", *dnsAddr,
	)

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("shutting down...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	httpServer.Shutdown(shutCtx)
	vm.Stop()
}

func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	return home + "/.loka/vm"
}

