package main

import (
	"testing"
)

func TestParseMount_BasicS3(t *testing.T) {
	m, err := parseMount("s3://bucket@/data")
	if err != nil {
		t.Fatalf("parseMount: %v", err)
	}
	if m.Provider != "s3" {
		t.Errorf("Provider = %q, want %q", m.Provider, "s3")
	}
	if m.Bucket != "bucket" {
		t.Errorf("Bucket = %q, want %q", m.Bucket, "bucket")
	}
	if m.MountPath != "/data" {
		t.Errorf("MountPath = %q, want %q", m.MountPath, "/data")
	}
	if m.Prefix != "" {
		t.Errorf("Prefix = %q, want empty", m.Prefix)
	}
	if m.ReadOnly {
		t.Error("ReadOnly = true, want false")
	}
}

func TestParseMount_PrefixAndReadOnly(t *testing.T) {
	m, err := parseMount("s3://bucket/prefix@/data:ro")
	if err != nil {
		t.Fatalf("parseMount: %v", err)
	}
	if m.Provider != "s3" {
		t.Errorf("Provider = %q, want %q", m.Provider, "s3")
	}
	if m.Bucket != "bucket" {
		t.Errorf("Bucket = %q, want %q", m.Bucket, "bucket")
	}
	if m.Prefix != "prefix" {
		t.Errorf("Prefix = %q, want %q", m.Prefix, "prefix")
	}
	if m.MountPath != "/data" {
		t.Errorf("MountPath = %q, want %q", m.MountPath, "/data")
	}
	if !m.ReadOnly {
		t.Error("ReadOnly = false, want true")
	}
}

func TestParseMount_GCSWithRegion(t *testing.T) {
	m, err := parseMount("gcs://bucket@/data?region=us-central1")
	if err != nil {
		t.Fatalf("parseMount: %v", err)
	}
	if m.Provider != "gcs" {
		t.Errorf("Provider = %q, want %q", m.Provider, "gcs")
	}
	if m.Region != "us-central1" {
		t.Errorf("Region = %q, want %q", m.Region, "us-central1")
	}
}

func TestParseMount_S3WithCredentials(t *testing.T) {
	m, err := parseMount("s3://bucket@/data?access_key_id=AKIA&secret_access_key=secret")
	if err != nil {
		t.Fatalf("parseMount: %v", err)
	}
	if m.Credentials == nil {
		t.Fatal("Credentials is nil")
	}
	if m.Credentials["access_key_id"] != "AKIA" {
		t.Errorf("access_key_id = %q, want %q", m.Credentials["access_key_id"], "AKIA")
	}
	if m.Credentials["secret_access_key"] != "secret" {
		t.Errorf("secret_access_key = %q, want %q", m.Credentials["secret_access_key"], "secret")
	}
}

func TestParseMount_CustomEndpoint(t *testing.T) {
	m, err := parseMount("s3://bucket@/data?endpoint=http://minio:9000")
	if err != nil {
		t.Fatalf("parseMount: %v", err)
	}
	if m.Endpoint != "http://minio:9000" {
		t.Errorf("Endpoint = %q, want %q", m.Endpoint, "http://minio:9000")
	}
}

func TestParseMount_InvalidNoScheme(t *testing.T) {
	_, err := parseMount("bucket@/data")
	if err == nil {
		t.Fatal("expected error for missing ://, got nil")
	}
}

func TestParseMount_InvalidNoAt(t *testing.T) {
	_, err := parseMount("s3://bucket/data")
	if err == nil {
		t.Fatal("expected error for missing @, got nil")
	}
}

func TestParsePortMap_Basic(t *testing.T) {
	pm, err := parsePortMap("8080:5000")
	if err != nil {
		t.Fatalf("parsePortMap: %v", err)
	}
	if pm.local != 8080 {
		t.Errorf("local = %d, want 8080", pm.local)
	}
	if pm.remote != 5000 {
		t.Errorf("remote = %d, want 5000", pm.remote)
	}
}

func TestParsePortMap_AutoAssign(t *testing.T) {
	pm, err := parsePortMap("0:3000")
	if err != nil {
		t.Fatalf("parsePortMap: %v", err)
	}
	if pm.local != 0 {
		t.Errorf("local = %d, want 0", pm.local)
	}
	if pm.remote != 3000 {
		t.Errorf("remote = %d, want 3000", pm.remote)
	}
}

func TestParsePortMap_InvalidLocalPort(t *testing.T) {
	_, err := parsePortMap("abc:5000")
	if err == nil {
		t.Fatal("expected error for non-numeric local port, got nil")
	}
}

func TestParsePortMap_MissingColon(t *testing.T) {
	_, err := parsePortMap("8080")
	if err == nil {
		t.Fatal("expected error for missing colon, got nil")
	}
}

func TestParsePortMap_InvalidRemotePort(t *testing.T) {
	_, err := parsePortMap("8080:abc")
	if err == nil {
		t.Fatal("expected error for non-numeric remote port, got nil")
	}
}
