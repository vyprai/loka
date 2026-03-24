package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// newTestService creates a fully populated loka.Service for testing.
func newTestService(name string, status loka.ServiceStatus, workerID string) *loka.Service {
	now := time.Now().Truncate(time.Second).UTC()
	return &loka.Service{
		ID:         uuid.New().String(),
		Name:       name,
		Status:     status,
		WorkerID:   workerID,
		ImageRef:   "nginx:latest",
		ImageID:    "sha256:abc123",
		RecipeName: "web",
		Command:    "nginx",
		Args:       []string{"-g", "daemon off;"},
		Env: map[string]string{
			"PORT":     "8080",
			"LOG_LEVEL": "info",
		},
		Workdir:        "/app",
		Port:           8080,
		VCPUs:          2,
		MemoryMB:       512,
		Routes:         []loka.ServiceRoute{{Subdomain: "api", Port: 8080, Protocol: "http"}},
		BundleKey:      "bundle-123",
		IdleTimeout:    300,
		HealthPath:     "/healthz",
		HealthInterval: 10,
		HealthTimeout:  5,
		HealthRetries:  3,
		Labels:         map[string]string{"env": "test"},
		Mounts:         []loka.VolumeMount{{Path: "/data", Provider: "volume", Name: "data-vol"}},
		Autoscale: &loka.AutoscaleConfig{
			Min:                1,
			Max:                5,
			TargetConcurrency:  100,
			ScaleUpThreshold:   0.8,
			ScaleDownThreshold: 0.3,
			Cooldown:           60,
		},
		SnapshotID:    "snap-001",
		Ready:         true,
		StatusMessage: "all good",
		LastActivity:  now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func TestServiceCreate(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("my-svc", loka.ServiceStatusRunning, "worker-1")
	err := s.Services().Create(ctx, svc)
	require.NoError(t, err)

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)
	require.Equal(t, svc.ID, got.ID)
	require.Equal(t, "my-svc", got.Name)
	require.Equal(t, loka.ServiceStatusRunning, got.Status)
	require.Equal(t, "worker-1", got.WorkerID)
	require.Equal(t, "nginx:latest", got.ImageRef)
	require.Equal(t, 8080, got.Port)
	require.Equal(t, 2, got.VCPUs)
	require.Equal(t, 512, got.MemoryMB)
	require.True(t, got.Ready)
}

func TestServiceGet(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("get-svc", loka.ServiceStatusDeploying, "worker-2")
	require.NoError(t, s.Services().Create(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)

	// Verify all scalar fields.
	require.Equal(t, svc.Name, got.Name)
	require.Equal(t, svc.Status, got.Status)
	require.Equal(t, svc.WorkerID, got.WorkerID)
	require.Equal(t, svc.ImageRef, got.ImageRef)
	require.Equal(t, svc.ImageID, got.ImageID)
	require.Equal(t, svc.RecipeName, got.RecipeName)
	require.Equal(t, svc.Command, got.Command)
	require.Equal(t, svc.Workdir, got.Workdir)
	require.Equal(t, svc.Port, got.Port)
	require.Equal(t, svc.BundleKey, got.BundleKey)
	require.Equal(t, svc.IdleTimeout, got.IdleTimeout)
	require.Equal(t, svc.HealthPath, got.HealthPath)
	require.Equal(t, svc.HealthInterval, got.HealthInterval)
	require.Equal(t, svc.HealthTimeout, got.HealthTimeout)
	require.Equal(t, svc.HealthRetries, got.HealthRetries)
	require.Equal(t, svc.SnapshotID, got.SnapshotID)
	require.Equal(t, svc.Ready, got.Ready)
	require.Equal(t, svc.StatusMessage, got.StatusMessage)

	// Verify JSON fields.
	require.Equal(t, svc.Args, got.Args)
	require.Equal(t, svc.Env, got.Env)
	require.Equal(t, svc.Routes, got.Routes)
	require.Equal(t, svc.Labels, got.Labels)
	require.Equal(t, svc.Mounts, got.Mounts)
	require.Equal(t, svc.Autoscale, got.Autoscale)
}

func TestServiceGetNotFound(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	_, err := s.Services().Get(ctx, "nonexistent-id")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestServiceUpdate(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("update-svc", loka.ServiceStatusDeploying, "worker-1")
	require.NoError(t, s.Services().Create(ctx, svc))

	// Modify fields.
	svc.Name = "updated-svc"
	svc.Status = loka.ServiceStatusRunning
	svc.Port = 9090
	svc.Ready = false
	svc.StatusMessage = "updated message"
	svc.Env["NEW_KEY"] = "new_value"
	svc.Args = append(svc.Args, "--verbose")

	require.NoError(t, s.Services().Update(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)
	require.Equal(t, "updated-svc", got.Name)
	require.Equal(t, loka.ServiceStatusRunning, got.Status)
	require.Equal(t, 9090, got.Port)
	require.False(t, got.Ready)
	require.Equal(t, "updated message", got.StatusMessage)
	require.Equal(t, "new_value", got.Env["NEW_KEY"])
	require.Contains(t, got.Args, "--verbose")
}

func TestServiceDelete(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("delete-svc", loka.ServiceStatusRunning, "worker-1")
	require.NoError(t, s.Services().Create(ctx, svc))

	require.NoError(t, s.Services().Delete(ctx, svc.ID))

	_, err := s.Services().Get(ctx, svc.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestServiceList(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc1 := newTestService("svc-1", loka.ServiceStatusRunning, "worker-1")
	svc2 := newTestService("svc-2", loka.ServiceStatusStopped, "worker-2")
	svc3 := newTestService("svc-3", loka.ServiceStatusDeploying, "worker-1")
	require.NoError(t, s.Services().Create(ctx, svc1))
	require.NoError(t, s.Services().Create(ctx, svc2))
	require.NoError(t, s.Services().Create(ctx, svc3))

	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{})
	require.NoError(t, err)
	require.Equal(t, 3, total)
	require.Len(t, svcs, 3)
}

func TestServiceListByStatus(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc1 := newTestService("running-1", loka.ServiceStatusRunning, "worker-1")
	svc2 := newTestService("running-2", loka.ServiceStatusRunning, "worker-2")
	svc3 := newTestService("stopped-1", loka.ServiceStatusStopped, "worker-1")
	require.NoError(t, s.Services().Create(ctx, svc1))
	require.NoError(t, s.Services().Create(ctx, svc2))
	require.NoError(t, s.Services().Create(ctx, svc3))

	running := loka.ServiceStatusRunning
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{Status: &running})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, svcs, 2)
	for _, svc := range svcs {
		require.Equal(t, loka.ServiceStatusRunning, svc.Status)
	}
}

func TestServiceListByWorker(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc1 := newTestService("w1-svc-1", loka.ServiceStatusRunning, "worker-1")
	svc2 := newTestService("w1-svc-2", loka.ServiceStatusStopped, "worker-1")
	svc3 := newTestService("w2-svc-1", loka.ServiceStatusRunning, "worker-2")
	require.NoError(t, s.Services().Create(ctx, svc1))
	require.NoError(t, s.Services().Create(ctx, svc2))
	require.NoError(t, s.Services().Create(ctx, svc3))

	wid := "worker-1"
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{WorkerID: &wid})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, svcs, 2)
	for _, svc := range svcs {
		require.Equal(t, "worker-1", svc.WorkerID)
	}
}

func TestServiceListByName(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc1 := newTestService("target-svc", loka.ServiceStatusRunning, "worker-1")
	svc2 := newTestService("other-svc", loka.ServiceStatusRunning, "worker-1")
	require.NoError(t, s.Services().Create(ctx, svc1))
	require.NoError(t, s.Services().Create(ctx, svc2))

	name := "target-svc"
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{Name: &name})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, svcs, 1)
	require.Equal(t, "target-svc", svcs[0].Name)
}

func TestServiceListPagination(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	// Create 5 services.
	for i := 0; i < 5; i++ {
		svc := newTestService("svc-"+uuid.New().String()[:4], loka.ServiceStatusRunning, "worker-1")
		// Stagger creation times to get deterministic ordering.
		svc.CreatedAt = time.Now().Add(time.Duration(i) * time.Second).Truncate(time.Second).UTC()
		require.NoError(t, s.Services().Create(ctx, svc))
	}

	// First page: limit 2, offset 0.
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{Limit: 2, Offset: 0})
	require.NoError(t, err)
	require.Equal(t, 5, total)
	require.Len(t, svcs, 2)

	// Second page: limit 2, offset 2.
	svcs2, total2, err := s.Services().List(ctx, store.ServiceFilter{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Equal(t, 5, total2)
	require.Len(t, svcs2, 2)

	// Pages should not overlap.
	require.NotEqual(t, svcs[0].ID, svcs2[0].ID)

	// Last page: limit 2, offset 4.
	svcs3, _, err := s.Services().List(ctx, store.ServiceFilter{Limit: 2, Offset: 4})
	require.NoError(t, err)
	require.Len(t, svcs3, 1)
}

func TestServiceListByWorkerMethod(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc1 := newTestService("w1-svc", loka.ServiceStatusRunning, "worker-1")
	svc2 := newTestService("w2-svc", loka.ServiceStatusRunning, "worker-2")
	require.NoError(t, s.Services().Create(ctx, svc1))
	require.NoError(t, s.Services().Create(ctx, svc2))

	svcs, err := s.Services().ListByWorker(ctx, "worker-1")
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	require.Equal(t, "worker-1", svcs[0].WorkerID)
}

func TestServiceJSONFields(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("json-svc", loka.ServiceStatusRunning, "worker-1")

	// Set up complex JSON fields.
	svc.Routes = []loka.ServiceRoute{
		{Subdomain: "api", Port: 8080, Protocol: "http"},
		{CustomDomain: "app.example.com", Port: 443, Protocol: "http"},
		{Subdomain: "grpc", Port: 9090, Protocol: "grpc"},
	}
	svc.Mounts = []loka.VolumeMount{
		{Path: "/data", Provider: "volume", Name: "data-vol", Access: "readwrite"},
		{Path: "/cache", Provider: "s3", Bucket: "my-bucket", Region: "us-east-1", Credentials: "${secret.aws}", Access: "readonly"},
		{Path: "/backup", Provider: "gcs", Bucket: "backup-bucket", Region: "us-central1"},
	}
	svc.Env = map[string]string{
		"PORT":          "8080",
		"LOG_LEVEL":     "debug",
		"DATABASE_URL":  "postgres://localhost/db",
		"NESTED_EQUALS": "key=value",
	}
	svc.Args = []string{"-c", "config.yaml", "--verbose", "--port=8080"}
	svc.Autoscale = &loka.AutoscaleConfig{
		Min:                2,
		Max:                10,
		TargetConcurrency:  200,
		ScaleUpThreshold:   0.75,
		ScaleDownThreshold: 0.25,
		Cooldown:           120,
	}
	svc.Labels = map[string]string{
		"env":     "production",
		"team":    "platform",
		"version": "v2.1.0",
	}

	require.NoError(t, s.Services().Create(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)

	// Routes round-trip.
	require.Len(t, got.Routes, 3)
	require.Equal(t, "api", got.Routes[0].Subdomain)
	require.Equal(t, "app.example.com", got.Routes[1].CustomDomain)
	require.Equal(t, 443, got.Routes[1].Port)
	require.Equal(t, "grpc", got.Routes[2].Protocol)

	// Mounts round-trip.
	require.Len(t, got.Mounts, 3)
	require.Equal(t, "volume", got.Mounts[0].Provider)
	require.Equal(t, "readwrite", got.Mounts[0].Access)
	require.Equal(t, "s3", got.Mounts[1].Provider)
	require.Equal(t, "my-bucket", got.Mounts[1].Bucket)
	require.Equal(t, "${secret.aws}", got.Mounts[1].Credentials)
	require.Equal(t, "gcs", got.Mounts[2].Provider)

	// Env round-trip.
	require.Len(t, got.Env, 4)
	require.Equal(t, "debug", got.Env["LOG_LEVEL"])
	require.Equal(t, "key=value", got.Env["NESTED_EQUALS"])

	// Args round-trip.
	require.Equal(t, []string{"-c", "config.yaml", "--verbose", "--port=8080"}, got.Args)

	// Autoscale round-trip.
	require.NotNil(t, got.Autoscale)
	require.Equal(t, 2, got.Autoscale.Min)
	require.Equal(t, 10, got.Autoscale.Max)
	require.Equal(t, 200, got.Autoscale.TargetConcurrency)
	require.InDelta(t, 0.75, got.Autoscale.ScaleUpThreshold, 0.001)
	require.InDelta(t, 0.25, got.Autoscale.ScaleDownThreshold, 0.001)
	require.Equal(t, 120, got.Autoscale.Cooldown)

	// Labels round-trip.
	require.Len(t, got.Labels, 3)
	require.Equal(t, "production", got.Labels["env"])
	require.Equal(t, "platform", got.Labels["team"])
}
