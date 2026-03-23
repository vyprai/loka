package provider

import (
	"context"
	"sort"
	"testing"
)

// mockProvider implements the Provider interface for testing.
type mockProvider struct {
	name string
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) Provision(_ context.Context, _ ProvisionOpts) ([]*WorkerInfo, error) {
	return nil, ErrNotSupported
}

func (m *mockProvider) Deprovision(_ context.Context, _ string) error {
	return ErrNotSupported
}

func (m *mockProvider) List(_ context.Context) ([]*WorkerInfo, error) {
	return nil, ErrNotSupported
}

func (m *mockProvider) WorkerStatus(_ context.Context, _ string) (WorkerInfraStatus, error) {
	return WorkerInfraError, ErrNotSupported
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{name: "aws"}
	r.Register(p)

	got, ok := r.Get("aws")
	if !ok {
		t.Fatal("expected to find provider 'aws'")
	}
	if got.Name() != "aws" {
		t.Fatalf("expected provider name 'aws', got %q", got.Name())
	}
}

func TestRegistry_GetNotFound(t *testing.T) {
	r := NewRegistry()

	_, ok := r.Get("nonexistent")
	if ok {
		t.Fatal("expected Get to return false for unregistered provider")
	}
}

func TestRegistry_ListEmpty(t *testing.T) {
	r := NewRegistry()
	names := r.List()
	if len(names) != 0 {
		t.Fatalf("expected empty list, got %v", names)
	}
}

func TestRegistry_ListMultiple(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockProvider{name: "aws"})
	r.Register(&mockProvider{name: "gcp"})
	r.Register(&mockProvider{name: "selfmanaged"})

	names := r.List()
	sort.Strings(names)

	expected := []string{"aws", "gcp", "selfmanaged"}
	if len(names) != len(expected) {
		t.Fatalf("expected %d providers, got %d", len(expected), len(names))
	}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("expected names[%d] = %q, got %q", i, expected[i], name)
		}
	}
}

func TestRegistry_RegisterDuplicateReplaces(t *testing.T) {
	r := NewRegistry()
	p1 := &mockProvider{name: "aws"}
	p2 := &mockProvider{name: "aws"}

	r.Register(p1)
	r.Register(p2)

	got, ok := r.Get("aws")
	if !ok {
		t.Fatal("expected to find provider 'aws'")
	}
	// The second registration should replace the first.
	if got != p2 {
		t.Fatal("expected the second registered provider to replace the first")
	}

	// List should still contain only one entry.
	names := r.List()
	if len(names) != 1 {
		t.Fatalf("expected 1 provider after duplicate register, got %d", len(names))
	}
}

func TestRegistry_GetAfterMultipleRegistrations(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockProvider{name: "aws"})
	r.Register(&mockProvider{name: "gcp"})

	// Verify each can be retrieved independently.
	if _, ok := r.Get("aws"); !ok {
		t.Error("expected to find 'aws'")
	}
	if _, ok := r.Get("gcp"); !ok {
		t.Error("expected to find 'gcp'")
	}
	if _, ok := r.Get("azure"); ok {
		t.Error("expected 'azure' to not be found")
	}
}
