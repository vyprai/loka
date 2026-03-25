package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/vyprai/loka/internal/worker"
)

func newWorkerCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Show worker layer cache stats",
		RunE: func(cmd *cobra.Command, args []string) error {
			cache := workerCache(cmd)
			stats := cache.Stats()

			if outputFmt == "json" {
				return printJSON(stats)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Total Size:\t%s\n", formatBytes(stats.TotalSize))
			fmt.Fprintf(w, "Layers:\t%d\n", stats.LayerCount)
			fmt.Fprintf(w, "Layer Packs:\t%d\n", stats.PackCount)
			fmt.Fprintf(w, "Snapshots:\t%d\n", stats.SnapCount)
			fmt.Fprintf(w, "Hits:\t%d\n", stats.HitCount)
			fmt.Fprintf(w, "Misses:\t%d\n", stats.MissCount)
			fmt.Fprintf(w, "Evictions:\t%d\n", stats.EvictCount)
			fmt.Fprintf(w, "Cache Dir:\t%s\n", cache.CacheDir())
			w.Flush()
			return nil
		},
	}

	cmd.AddCommand(newWorkerCacheCleanCmd())
	return cmd
}

func newWorkerCacheCleanCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Clean the worker layer cache",
		Long:  "Evict expired and over-limit entries. Use --all to clear everything.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cache := workerCache(cmd)

			if all {
				if err := cache.CleanAll(); err != nil {
					return fmt.Errorf("clean all: %w", err)
				}
				fmt.Println("Cache cleared")
				return nil
			}

			cache.Clean()
			stats := cache.Stats()
			fmt.Printf("Cache cleaned (remaining: %s, %d layers, %d packs, %d snapshots)\n",
				formatBytes(stats.TotalSize), stats.LayerCount, stats.PackCount, stats.SnapCount)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Clear all cached files")
	return cmd
}

// workerCache creates a Cache instance pointing at the local worker cache directory.
// The cache is read-only (no object store backend) — used only for stats and cleanup.
func workerCache(_ *cobra.Command) *worker.Cache {
	dataDir := os.Getenv("LOKA_DATA_DIR")
	if dataDir == "" {
		dataDir = "/var/lib/loka"
	}
	cacheDir := filepath.Join(dataDir, "cache")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return worker.NewCache(cacheDir, nil, "", logger)
}

func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
