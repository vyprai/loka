package volrepl

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/worker/volsync"
)

// Replicator manages block volume replication for a single worker.
// It tracks which volumes this worker is primary for (push to replicas)
// and which it is a replica for (pull from primary).
type Replicator struct {
	dataDir  string
	workerID string
	logger   *slog.Logger
	agent    *volsync.Agent // The volsync agent for fsnotify + sync.

	mu       sync.Mutex
	volumes  map[string]*replVolume // volume name → replication state
	ctx      context.Context
	cancel   context.CancelFunc
}

type replVolume struct {
	name        string
	role        string // "primary" or "replica"
	peerAddrs   []string // addresses of peer workers
	syncCancel  context.CancelFunc
}

// NewReplicator creates a volume replicator for this worker.
func NewReplicator(dataDir, workerID string, agent *volsync.Agent, logger *slog.Logger) *Replicator {
	ctx, cancel := context.WithCancel(context.Background())
	return &Replicator{
		dataDir:  dataDir,
		workerID: workerID,
		logger:   logger,
		agent:    agent,
		volumes:  make(map[string]*replVolume),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Stop shuts down all replication loops.
func (r *Replicator) Stop() {
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rv := range r.volumes {
		if rv.syncCancel != nil {
			rv.syncCancel()
		}
	}
}

// ServePrimary starts serving a volume as primary — pushes changes to replica peers.
// It registers PeerSyncTargets with the volsync agent so that fsnotify-triggered
// writes are automatically forwarded to replicas.
func (r *Replicator) ServePrimary(name string, replicaAddrs []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Ensure volume directory exists.
	volDir := filepath.Join(r.dataDir, "volumes", name)
	os.MkdirAll(volDir, 0o755)

	// Remove old targets for this volume.
	r.agent.RemoveTargets(name)

	// Register peer sync targets for each replica.
	for _, addr := range replicaAddrs {
		target := NewPeerSyncTarget(addr)
		r.agent.AddTarget(name, target)
	}

	// Start watching the volume (the agent handles fsnotify + push via targets).
	if err := r.agent.WatchVolume(name); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(r.ctx)
	r.volumes[name] = &replVolume{
		name:       name,
		role:       "primary",
		peerAddrs:  replicaAddrs,
		syncCancel: cancel,
	}

	r.logger.Info("serving volume as primary", "volume", name, "replicas", replicaAddrs)

	// Background: periodic manifest push to ensure replicas stay in sync.
	go r.primaryReconcileLoop(ctx, name, replicaAddrs)

	return nil
}

// ServeReplica starts replicating a volume from the primary worker.
// Pulls the full volume on initial sync, then periodically reconciles.
func (r *Replicator) ServeReplica(name string, primaryAddr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Ensure volume directory exists.
	volDir := filepath.Join(r.dataDir, "volumes", name)
	os.MkdirAll(volDir, 0o755)

	ctx, cancel := context.WithCancel(r.ctx)
	r.volumes[name] = &replVolume{
		name:       name,
		role:       "replica",
		peerAddrs:  []string{primaryAddr},
		syncCancel: cancel,
	}

	r.logger.Info("serving volume as replica", "volume", name, "primary", primaryAddr)

	// Initial full sync + periodic reconciliation.
	go r.replicaPullLoop(ctx, name, primaryAddr)

	return nil
}

// StopVolume stops replication for a volume.
func (r *Replicator) StopVolume(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if rv, ok := r.volumes[name]; ok {
		if rv.syncCancel != nil {
			rv.syncCancel()
		}
		r.agent.RemoveTargets(name)
		r.agent.UnwatchVolume(name)
		delete(r.volumes, name)
		r.logger.Info("stopped volume replication", "volume", name)
	}
}

// primaryReconcileLoop does nothing extra for now — the volsync agent's
// watchLoop + targets already handle push-on-write. This loop exists for
// future health checks or manifest reconciliation.
func (r *Replicator) primaryReconcileLoop(ctx context.Context, name string, replicaAddrs []string) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Trigger a full reconcile via the volsync agent (catches missed fsnotify).
			// The agent's reconcileVolume is called internally every 5 min already,
			// but we can also force a sync-to-remote here.
			if err := r.agent.SyncToRemote(name); err != nil {
				r.logger.Warn("primary reconcile failed", "volume", name, "error", err)
			}
		}
	}
}

// replicaPullLoop periodically pulls changes from the primary worker.
func (r *Replicator) replicaPullLoop(ctx context.Context, name, primaryAddr string) {
	client := NewClient(primaryAddr)
	volDir := filepath.Join(r.dataDir, "volumes", name)

	// Initial full sync.
	if err := r.pullFromPrimary(ctx, client, name, volDir); err != nil {
		r.logger.Warn("initial replica sync failed", "volume", name, "error", err)
	}

	// Periodic reconciliation.
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.pullFromPrimary(ctx, client, name, volDir); err != nil {
				r.logger.Warn("replica sync failed", "volume", name, "error", err)
			}
		}
	}
}

// pullFromPrimary does a manifest-based diff and downloads changed files.
func (r *Replicator) pullFromPrimary(ctx context.Context, client *Client, name, volDir string) error {
	remoteManifest, err := client.FetchManifest(ctx, name)
	if err != nil {
		return err
	}

	localManifest := volsync.BuildLocalManifest(volDir)

	// Download new/changed files.
	count := 0
	for relPath, remoteEntry := range remoteManifest.Files {
		localEntry, exists := localManifest.Files[relPath]
		if exists && localEntry.SHA256 == remoteEntry.SHA256 {
			continue
		}
		localPath := filepath.Join(volDir, relPath)
		if err := client.DownloadFile(ctx, name, relPath, localPath); err != nil {
			r.logger.Warn("replica download failed", "volume", name, "file", relPath, "error", err)
			continue
		}
		count++
	}

	// Remove files that exist locally but not on primary.
	for relPath := range localManifest.Files {
		if _, exists := remoteManifest.Files[relPath]; !exists {
			os.Remove(filepath.Join(volDir, relPath))
			count++
		}
	}

	if count > 0 {
		r.logger.Info("replica synced from primary", "volume", name, "changes", count)
	}
	return nil
}

// VolumePath returns the local path for a volume.
func (r *Replicator) VolumePath(name string) string {
	return filepath.Join(r.dataDir, "volumes", name)
}
