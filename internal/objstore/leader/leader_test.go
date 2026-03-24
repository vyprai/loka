package leader

import (
	"testing"
)

func TestSplitHostPort_IPv4(t *testing.T) {
	host, port, err := splitHostPort("192.168.1.1:6840")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "192.168.1.1" {
		t.Errorf("host = %q, want %q", host, "192.168.1.1")
	}
	if port != "6840" {
		t.Errorf("port = %q, want %q", port, "6840")
	}
}

func TestSplitHostPort_IPv6(t *testing.T) {
	host, port, err := splitHostPort("[::1]:6840")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "::1" {
		t.Errorf("host = %q, want %q", host, "::1")
	}
	if port != "6840" {
		t.Errorf("port = %q, want %q", port, "6840")
	}
}

func TestSplitHostPort_IPv6Full(t *testing.T) {
	host, port, err := splitHostPort("[2001:db8::1]:8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "2001:db8::1" {
		t.Errorf("host = %q, want %q", host, "2001:db8::1")
	}
	if port != "8080" {
		t.Errorf("port = %q, want %q", port, "8080")
	}
}

func TestSplitHostPort_NoPort(t *testing.T) {
	host, port, _ := splitHostPort("hostname-only")
	if host != "hostname-only" {
		t.Errorf("host = %q, want %q", host, "hostname-only")
	}
	if port != "" {
		t.Errorf("port = %q, want empty", port)
	}
}

func TestSplitHostPort_Hostname(t *testing.T) {
	host, port, err := splitHostPort("leader.example.com:6840")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "leader.example.com" {
		t.Errorf("host = %q, want %q", host, "leader.example.com")
	}
	if port != "6840" {
		t.Errorf("port = %q, want %q", port, "6840")
	}
}
