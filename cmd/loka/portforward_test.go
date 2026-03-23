package main

import (
	"testing"
)

func TestParsePortMap_Valid(t *testing.T) {
	tests := []struct {
		input      string
		wantLocal  int
		wantRemote int
	}{
		{"8080:5000", 8080, 5000},
		{"0:3000", 0, 3000},
		{"443:443", 443, 443},
		{"9090:80", 9090, 80},
		{"65535:1", 65535, 1},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			pm, err := parsePortMap(tt.input)
			if err != nil {
				t.Fatalf("parsePortMap(%q) unexpected error: %v", tt.input, err)
			}
			if pm.local != tt.wantLocal {
				t.Errorf("parsePortMap(%q).local = %d, want %d", tt.input, pm.local, tt.wantLocal)
			}
			if pm.remote != tt.wantRemote {
				t.Errorf("parsePortMap(%q).remote = %d, want %d", tt.input, pm.remote, tt.wantRemote)
			}
		})
	}
}

func TestParsePortMap_Invalid(t *testing.T) {
	tests := []struct {
		input string
		desc  string
	}{
		{"abc:5000", "non-numeric local port"},
		{"8080", "missing colon separator"},
		{"8080:abc", "non-numeric remote port"},
		{":", "empty ports on both sides"},
		{"", "empty string"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			_, err := parsePortMap(tt.input)
			if err == nil {
				t.Fatalf("parsePortMap(%q) expected error, got nil", tt.input)
			}
		})
	}
}
