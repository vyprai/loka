package ha

import (
	"strings"
	"testing"
)

func TestOpen_Local(t *testing.T) {
	coord, err := Open(Config{Type: "local"})
	if err != nil {
		t.Fatalf("Open(local) failed: %v", err)
	}
	if coord == nil {
		t.Fatal("expected non-nil coordinator")
	}
	defer coord.Close()
}

func TestOpen_UnknownType(t *testing.T) {
	_, err := Open(Config{Type: "redis"})
	if err == nil {
		t.Fatal("expected error for unknown coordinator type")
	}
	if !strings.Contains(err.Error(), "unknown coordinator type") {
		t.Errorf("expected descriptive error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "redis") {
		t.Errorf("expected error to mention the bad type 'redis', got: %v", err)
	}
}

func TestOpen_EmptyType(t *testing.T) {
	_, err := Open(Config{Type: ""})
	if err == nil {
		t.Fatal("expected error for empty coordinator type")
	}
}

func TestRegisterFactory_RoundTrip(t *testing.T) {
	called := false
	RegisterFactory("test-custom", func(cfg Config) (Coordinator, error) {
		called = true
		return NewLocalCoordinator(), nil
	})

	coord, err := Open(Config{Type: "test-custom"})
	if err != nil {
		t.Fatalf("Open(test-custom) failed: %v", err)
	}
	if !called {
		t.Fatal("expected custom factory to be called")
	}
	defer coord.Close()
}
