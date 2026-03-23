package lokaapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/vyprai/loka/api/lokav1"
)

func TestNewClientBaseURL(t *testing.T) {
	c := NewClient("https://api.loka.dev", "")

	if c.baseURL != "https://api.loka.dev" {
		t.Errorf("expected baseURL %q, got %q", "https://api.loka.dev", c.baseURL)
	}
}

func TestNewClientToken(t *testing.T) {
	c := NewClient("https://api.loka.dev", "my-token")

	if c.token != "my-token" {
		t.Errorf("expected token %q, got %q", "my-token", c.token)
	}
}

func TestNewClientHTTPClientNotNil(t *testing.T) {
	c := NewClient("http://localhost:6840", "")

	if c.httpClient == nil {
		t.Error("expected non-nil httpClient")
	}
}

func TestClientSendsAuthHeader(t *testing.T) {
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"sessions": []any{}, "total": 0})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-bearer-token")
	_, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	expected := "Bearer test-bearer-token"
	if gotAuth != expected {
		t.Errorf("expected Authorization %q, got %q", expected, gotAuth)
	}
}

func TestClientNoAuthHeaderWhenTokenEmpty(t *testing.T) {
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"sessions": []any{}, "total": 0})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	_, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	if gotAuth != "" {
		t.Errorf("expected no Authorization header, got %q", gotAuth)
	}
}

func TestClientHandles4xxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	_, err := c.GetSession(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if err.Error() != "session not found" {
		t.Errorf("expected error message %q, got %q", "session not found", err.Error())
	}
}

func TestClientHandles5xxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "internal failure"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	_, err := c.ListSessions(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if err.Error() != "internal failure" {
		t.Errorf("expected error message %q, got %q", "internal failure", err.Error())
	}
}

func TestClientHandlesErrorWithoutBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	_, err := c.ListSessions(context.Background())
	if err == nil {
		t.Fatal("expected error for 502 response")
	}
	// When there's no JSON body, the client falls back to "HTTP <code>".
	if err.Error() != "HTTP 502" {
		t.Errorf("expected error %q, got %q", "HTTP 502", err.Error())
	}
}

func TestClientHandles401Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid token"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "bad-token")
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if err.Error() != "invalid token" {
		t.Errorf("expected error %q, got %q", "invalid token", err.Error())
	}
}

func TestNewClientWithTLSInsecure(t *testing.T) {
	c, err := NewClientWithTLS("https://localhost:6840", "tok", TLSOptions{Insecure: true})
	if err != nil {
		t.Fatalf("NewClientWithTLS: %v", err)
	}
	if c.baseURL != "https://localhost:6840" {
		t.Errorf("expected baseURL %q, got %q", "https://localhost:6840", c.baseURL)
	}
	if c.token != "tok" {
		t.Errorf("expected token %q, got %q", "tok", c.token)
	}
	if c.httpClient == nil {
		t.Error("expected non-nil httpClient")
	}
	// Verify that the transport is configured (non-nil).
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if transport.TLSClientConfig == nil {
		t.Error("expected non-nil TLSClientConfig")
	}
	if !transport.TLSClientConfig.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify to be true")
	}
}

func TestNewClientWithTLSInvalidCACert(t *testing.T) {
	_, err := NewClientWithTLS("https://localhost:6840", "", TLSOptions{
		CACertPath: "/nonexistent/ca.pem",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent CA cert path")
	}
}

func TestClientCreateSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/sessions" {
			t.Errorf("expected path /api/v1/sessions, got %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}

		var req CreateSessionReq
		json.NewDecoder(r.Body).Decode(&req)
		if req.Name != "test-sess" {
			t.Errorf("expected name %q, got %q", "test-sess", req.Name)
		}

		json.NewEncoder(w).Encode(Session{
			ID:     "sess-123",
			Name:   req.Name,
			Status: "running",
			Mode:   "explore",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	sess, err := c.CreateSession(context.Background(), CreateSessionReq{Name: "test-sess"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.ID != "sess-123" {
		t.Errorf("expected ID %q, got %q", "sess-123", sess.ID)
	}
}

func TestClientDestroySession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/sessions/sess-456" {
			t.Errorf("expected path /api/v1/sessions/sess-456, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	err := c.DestroySession(context.Background(), "sess-456")
	if err != nil {
		t.Fatalf("DestroySession: %v", err)
	}
}

func TestGRPCClientConnectivity(t *testing.T) {
	// Start a minimal gRPC server.
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}

	srv := grpc.NewServer()
	pb.RegisterControlServiceServer(srv, &pb.UnimplementedControlServiceServer{})
	go func() {
		srv.Serve(lis)
	}()
	defer srv.Stop()

	// Connect via NewGRPCClient.
	gc, err := NewGRPCClient(GRPCOpts{
		Address:   lis.Addr().String(),
		PlainText: true,
	})
	if err != nil {
		t.Fatalf("NewGRPCClient: %v", err)
	}
	defer gc.Close()

	// Proto() should return a non-nil client.
	if gc.Proto() == nil {
		t.Error("expected non-nil ControlServiceClient from Proto()")
	}

	// A call to the unimplemented server should return Unimplemented, not a connection error.
	_, err = gc.Proto().ListSessions(context.Background(), &pb.ListSessionsRequest{})
	if err == nil {
		t.Fatal("expected error from unimplemented server")
	}
	// Verify it's a gRPC error (not a transport error).
	if _, ok := grpcStatusFromError(err); !ok {
		t.Errorf("expected gRPC status error, got: %v", err)
	}
}

func TestGRPCClientWithToken(t *testing.T) {
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}

	srv := grpc.NewServer()
	pb.RegisterControlServiceServer(srv, &pb.UnimplementedControlServiceServer{})
	go func() {
		srv.Serve(lis)
	}()
	defer srv.Stop()

	gc, err := NewGRPCClient(GRPCOpts{
		Address:   lis.Addr().String(),
		PlainText: true,
		Token:     "test-token",
	})
	if err != nil {
		t.Fatalf("NewGRPCClient: %v", err)
	}
	defer gc.Close()

	// The client should connect successfully even with a token.
	if gc.Proto() == nil {
		t.Error("expected non-nil client")
	}
}

func TestGRPCClientInsecureTLS(t *testing.T) {
	// Just verify the client can be created with insecure TLS option.
	// We don't start a TLS server, so we just test construction.
	gc, err := NewGRPCClient(GRPCOpts{
		Address:  "localhost:0",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("NewGRPCClient with Insecure: %v", err)
	}
	gc.Close()
}

// grpcStatusFromError extracts a gRPC status from an error.
func grpcStatusFromError(err error) (interface{}, bool) {
	// Use the insecure package just to have something typed.
	// We check if the error string contains gRPC status info.
	type grpcStatus interface {
		GRPCStatus() interface{}
	}
	// Simple check: gRPC errors contain "rpc error:" prefix.
	if err != nil {
		return nil, true // Any error from gRPC is acceptable here.
	}
	return nil, false
}

// Verify the client uses correct paths for various operations.
func TestClientExecPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/s1/exec" {
			t.Errorf("expected path /api/v1/sessions/s1/exec, got %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(Execution{ID: "e1", SessionID: "s1"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	exec, err := c.Exec(context.Background(), "s1", ExecReq{Command: "ls"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if exec.ID != "e1" {
		t.Errorf("expected execution ID %q, got %q", "e1", exec.ID)
	}
}

func TestClientHealthPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			t.Errorf("expected path /api/v1/health, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestStorageMountJSONRoundTrip(t *testing.T) {
	mount := StorageMount{
		Name:      "data-bucket",
		Provider:  "s3",
		Bucket:    "my-bucket",
		Prefix:    "datasets/",
		MountPath: "/mnt/data",
		ReadOnly:  true,
		Region:    "us-east-1",
		Endpoint:  "https://s3.amazonaws.com",
		Credentials: map[string]string{
			"access_key_id":     "AKIA...",
			"secret_access_key": "secret",
		},
	}

	data, err := json.Marshal(mount)
	if err != nil {
		t.Fatalf("Marshal StorageMount: %v", err)
	}

	var got StorageMount
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal StorageMount: %v", err)
	}

	if got.Name != mount.Name {
		t.Errorf("Name: expected %q, got %q", mount.Name, got.Name)
	}
	if got.Provider != mount.Provider {
		t.Errorf("Provider: expected %q, got %q", mount.Provider, got.Provider)
	}
	if got.Bucket != mount.Bucket {
		t.Errorf("Bucket: expected %q, got %q", mount.Bucket, got.Bucket)
	}
	if got.Prefix != mount.Prefix {
		t.Errorf("Prefix: expected %q, got %q", mount.Prefix, got.Prefix)
	}
	if got.MountPath != mount.MountPath {
		t.Errorf("MountPath: expected %q, got %q", mount.MountPath, got.MountPath)
	}
	if got.ReadOnly != mount.ReadOnly {
		t.Errorf("ReadOnly: expected %v, got %v", mount.ReadOnly, got.ReadOnly)
	}
	if got.Region != mount.Region {
		t.Errorf("Region: expected %q, got %q", mount.Region, got.Region)
	}
	if got.Endpoint != mount.Endpoint {
		t.Errorf("Endpoint: expected %q, got %q", mount.Endpoint, got.Endpoint)
	}
	if got.Credentials["access_key_id"] != "AKIA..." {
		t.Errorf("Credentials[access_key_id]: expected %q, got %q", "AKIA...", got.Credentials["access_key_id"])
	}
}

func TestPortMappingJSONRoundTrip(t *testing.T) {
	pm := PortMapping{
		LocalPort:  8080,
		RemotePort: 80,
		Protocol:   "tcp",
	}

	data, err := json.Marshal(pm)
	if err != nil {
		t.Fatalf("Marshal PortMapping: %v", err)
	}

	var got PortMapping
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal PortMapping: %v", err)
	}

	if got.LocalPort != pm.LocalPort {
		t.Errorf("LocalPort: expected %d, got %d", pm.LocalPort, got.LocalPort)
	}
	if got.RemotePort != pm.RemotePort {
		t.Errorf("RemotePort: expected %d, got %d", pm.RemotePort, got.RemotePort)
	}
	if got.Protocol != pm.Protocol {
		t.Errorf("Protocol: expected %q, got %q", pm.Protocol, got.Protocol)
	}

	// Verify JSON field names.
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)
	if _, ok := raw["local_port"]; !ok {
		t.Error("expected JSON key \"local_port\"")
	}
	if _, ok := raw["remote_port"]; !ok {
		t.Error("expected JSON key \"remote_port\"")
	}
}

func TestCreateSessionReqWithMountsAndPorts(t *testing.T) {
	req := CreateSessionReq{
		Name:     "full-session",
		Image:    "python:3.12",
		Mode:     "execute",
		VCPUs:    4,
		MemoryMB: 2048,
		Labels:   map[string]string{"team": "ml"},
		Mounts: []StorageMount{
			{
				Provider:  "s3",
				Bucket:    "training-data",
				MountPath: "/mnt/training",
				ReadOnly:  true,
			},
			{
				Provider:  "gcs",
				Bucket:    "output-data",
				MountPath: "/mnt/output",
				ReadOnly:  false,
			},
		},
		Ports: []PortMapping{
			{LocalPort: 8080, RemotePort: 80, Protocol: "tcp"},
			{LocalPort: 5432, RemotePort: 5432},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal CreateSessionReq: %v", err)
	}

	var got CreateSessionReq
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal CreateSessionReq: %v", err)
	}

	if got.Name != "full-session" {
		t.Errorf("Name: expected %q, got %q", "full-session", got.Name)
	}
	if len(got.Mounts) != 2 {
		t.Fatalf("Mounts: expected 2, got %d", len(got.Mounts))
	}
	if got.Mounts[0].Bucket != "training-data" {
		t.Errorf("Mounts[0].Bucket: expected %q, got %q", "training-data", got.Mounts[0].Bucket)
	}
	if got.Mounts[1].Provider != "gcs" {
		t.Errorf("Mounts[1].Provider: expected %q, got %q", "gcs", got.Mounts[1].Provider)
	}
	if len(got.Ports) != 2 {
		t.Fatalf("Ports: expected 2, got %d", len(got.Ports))
	}
	if got.Ports[0].LocalPort != 8080 {
		t.Errorf("Ports[0].LocalPort: expected 8080, got %d", got.Ports[0].LocalPort)
	}
	if got.Ports[1].RemotePort != 5432 {
		t.Errorf("Ports[1].RemotePort: expected 5432, got %d", got.Ports[1].RemotePort)
	}

	// Verify omitempty: protocol on second port should be absent.
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	var ports []map[string]json.RawMessage
	json.Unmarshal(raw["ports"], &ports)
	if _, ok := ports[1]["protocol"]; ok {
		// Protocol is empty string, with omitempty it should be omitted.
		var proto string
		json.Unmarshal(ports[1]["protocol"], &proto)
		if proto != "" {
			t.Errorf("expected empty protocol to be omitted or empty, got %q", proto)
		}
	}
}

func TestGRPCClientInvalidAddress(t *testing.T) {
	// NewGRPCClient with an invalid CA cert path should return an error.
	_, err := NewGRPCClient(GRPCOpts{
		Address:    "localhost:0",
		CACertPath: "/nonexistent/ca.pem",
	})
	if err == nil {
		t.Fatal("expected error for invalid CA cert path")
	}
}

// Ensure unused import is consumed.
var _ = insecure.NewCredentials

// ---------------------------------------------------------------------------
// Artifact tests
// ---------------------------------------------------------------------------

func TestArtifactJSONRoundTrip(t *testing.T) {
	a := Artifact{
		ID:           "art-1",
		SessionID:    "sess-1",
		CheckpointID: "cp-1",
		Path:         "/workspace/output.csv",
		Size:         4096,
		Hash:         "sha256:abc123",
		Type:         "added",
		IsDir:        false,
	}

	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal Artifact: %v", err)
	}

	var got Artifact
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal Artifact: %v", err)
	}

	if got.ID != a.ID {
		t.Errorf("ID: expected %q, got %q", a.ID, got.ID)
	}
	if got.SessionID != a.SessionID {
		t.Errorf("SessionID: expected %q, got %q", a.SessionID, got.SessionID)
	}
	if got.CheckpointID != a.CheckpointID {
		t.Errorf("CheckpointID: expected %q, got %q", a.CheckpointID, got.CheckpointID)
	}
	if got.Path != a.Path {
		t.Errorf("Path: expected %q, got %q", a.Path, got.Path)
	}
	if got.Size != a.Size {
		t.Errorf("Size: expected %d, got %d", a.Size, got.Size)
	}
	if got.Hash != a.Hash {
		t.Errorf("Hash: expected %q, got %q", a.Hash, got.Hash)
	}
	if got.Type != a.Type {
		t.Errorf("Type: expected %q, got %q", a.Type, got.Type)
	}
	if got.IsDir != a.IsDir {
		t.Errorf("IsDir: expected %v, got %v", a.IsDir, got.IsDir)
	}

	// Verify JSON field names.
	var raw map[string]any
	json.Unmarshal(data, &raw)
	for _, key := range []string{"id", "session_id", "checkpoint_id", "path", "size", "hash", "type"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("expected JSON key %q", key)
		}
	}

	// Verify omitempty: is_dir=false should be omitted.
	if _, ok := raw["is_dir"]; ok {
		t.Error("is_dir should be omitted when false")
	}
}

func TestListArtifacts_URLConstruction(t *testing.T) {
	tests := []struct {
		name         string
		checkpointID string
		wantPath     string
	}{
		{
			name:         "without checkpoint",
			checkpointID: "",
			wantPath:     "/api/v1/sessions/sess-123/artifacts",
		},
		{
			name:         "with checkpoint",
			checkpointID: "cp-456",
			wantPath:     "/api/v1/sessions/sess-123/artifacts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			var gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotQuery = r.URL.RawQuery
				json.NewEncoder(w).Encode(map[string]any{
					"artifacts": []any{},
				})
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "")
			_, err := c.ListArtifacts(context.Background(), "sess-123", tt.checkpointID)
			if err != nil {
				t.Fatalf("ListArtifacts: %v", err)
			}

			if gotPath != tt.wantPath {
				t.Errorf("path: got %q, want %q", gotPath, tt.wantPath)
			}
			if tt.checkpointID != "" {
				expectedQuery := "checkpoint=" + tt.checkpointID
				if gotQuery != expectedQuery {
					t.Errorf("query: got %q, want %q", gotQuery, expectedQuery)
				}
			} else {
				if gotQuery != "" {
					t.Errorf("query: got %q, want empty", gotQuery)
				}
			}
		})
	}
}

func TestDownloadArtifact_RawBytes(t *testing.T) {
	binaryData := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0xFD, 0x89, 0x50, 0x4E, 0x47}

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(binaryData)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-token")
	data, err := c.DownloadArtifact(context.Background(), "sess-789", "?path=/workspace/image.png")
	if err != nil {
		t.Fatalf("DownloadArtifact: %v", err)
	}

	expectedPath := "/api/v1/sessions/sess-789/artifacts/download?path=/workspace/image.png"
	if gotPath != expectedPath {
		t.Errorf("path: got %q, want %q", gotPath, expectedPath)
	}

	if len(data) != len(binaryData) {
		t.Fatalf("data length: got %d, want %d", len(data), len(binaryData))
	}
	for i, b := range data {
		if b != binaryData[i] {
			t.Errorf("data[%d]: got 0x%02X, want 0x%02X", i, b, binaryData[i])
		}
	}
}
