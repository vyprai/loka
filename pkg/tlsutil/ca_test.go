package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateCA(t *testing.T) {
	dir := t.TempDir()

	// First call generates files.
	certPath, keyPath, err := GenerateCA(dir)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	if certPath != filepath.Join(dir, "ca.crt") {
		t.Errorf("certPath = %q, want %q", certPath, filepath.Join(dir, "ca.crt"))
	}
	if keyPath != filepath.Join(dir, "ca.key") {
		t.Errorf("keyPath = %q, want %q", keyPath, filepath.Join(dir, "ca.key"))
	}

	// Verify files exist.
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("ca.crt not created: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("ca.key not created: %v", err)
	}

	// Parse the certificate and verify it is a CA.
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to decode ca.crt PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse ca.crt: %v", err)
	}
	if !cert.IsCA {
		t.Error("certificate IsCA = false, want true")
	}
	if cert.Subject.CommonName != "LOKA Auto CA" {
		t.Errorf("CommonName = %q, want %q", cert.Subject.CommonName, "LOKA Auto CA")
	}
}

func TestGenerateCA_Idempotent(t *testing.T) {
	dir := t.TempDir()

	certPath1, keyPath1, err := GenerateCA(dir)
	if err != nil {
		t.Fatalf("first GenerateCA: %v", err)
	}
	stat1, _ := os.Stat(certPath1)

	// Second call should return same paths without regenerating.
	certPath2, keyPath2, err := GenerateCA(dir)
	if err != nil {
		t.Fatalf("second GenerateCA: %v", err)
	}
	if certPath1 != certPath2 || keyPath1 != keyPath2 {
		t.Error("paths differ on second call")
	}
	stat2, _ := os.Stat(certPath2)
	if !stat1.ModTime().Equal(stat2.ModTime()) {
		t.Error("ca.crt was regenerated on second call (mod time changed)")
	}
}

func TestGenerateServerCert(t *testing.T) {
	caDir := t.TempDir()
	caCertPath, caKeyPath, err := GenerateCA(caDir)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	serverDir := t.TempDir()
	extraSANs := []string{"myhost.local", "10.0.0.1"}
	certPath, keyPath, err := GenerateServerCert(caCertPath, caKeyPath, serverDir, extraSANs)
	if err != nil {
		t.Fatalf("GenerateServerCert: %v", err)
	}

	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("server.crt not created: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("server.key not created: %v", err)
	}

	// Parse the server cert.
	certPEM, _ := os.ReadFile(certPath)
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to decode server.crt PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse server.crt: %v", err)
	}

	// Verify SANs include localhost and custom SAN.
	foundLocalhost := false
	foundCustom := false
	for _, dns := range cert.DNSNames {
		if dns == "localhost" {
			foundLocalhost = true
		}
		if dns == "myhost.local" {
			foundCustom = true
		}
	}
	if !foundLocalhost {
		t.Error("server cert missing DNS SAN 'localhost'")
	}
	if !foundCustom {
		t.Error("server cert missing DNS SAN 'myhost.local'")
	}

	// Verify IP SAN 10.0.0.1 is present.
	foundIP := false
	for _, ip := range cert.IPAddresses {
		if ip.String() == "10.0.0.1" {
			foundIP = true
		}
	}
	if !foundIP {
		t.Error("server cert missing IP SAN 10.0.0.1")
	}

	// Verify the server cert validates against the CA.
	caPool, err := LoadCA(caCertPath)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	opts := x509.VerifyOptions{
		Roots:     caPool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, err := cert.Verify(opts); err != nil {
		t.Errorf("server cert failed CA verification: %v", err)
	}
}

func TestLoadCA(t *testing.T) {
	dir := t.TempDir()
	caCertPath, _, err := GenerateCA(dir)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	pool, err := LoadCA(caCertPath)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if pool == nil {
		t.Fatal("LoadCA returned nil pool")
	}
}

func TestLoadCA_MissingFile(t *testing.T) {
	_, err := LoadCA("/nonexistent/ca.crt")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadCA_InvalidPEM(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.crt")
	if err := os.WriteFile(badPath, []byte("not a PEM"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCA(badPath)
	if err == nil {
		t.Fatal("expected error for invalid PEM, got nil")
	}
}

func TestLoadServerTLS(t *testing.T) {
	caDir := t.TempDir()
	caCertPath, caKeyPath, err := GenerateCA(caDir)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	serverDir := t.TempDir()
	certPath, keyPath, err := GenerateServerCert(caCertPath, caKeyPath, serverDir, nil)
	if err != nil {
		t.Fatalf("GenerateServerCert: %v", err)
	}

	// With CA cert (client auth enabled).
	cfg, err := LoadServerTLS(certPath, keyPath, caCertPath)
	if err != nil {
		t.Fatalf("LoadServerTLS with CA: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want %d", cfg.MinVersion, tls.VersionTLS12)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates count = %d, want 1", len(cfg.Certificates))
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs is nil when CA cert provided")
	}
	if cfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Errorf("ClientAuth = %d, want VerifyClientCertIfGiven", cfg.ClientAuth)
	}
}

func TestLoadServerTLS_WithoutCA(t *testing.T) {
	caDir := t.TempDir()
	caCertPath, caKeyPath, err := GenerateCA(caDir)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	serverDir := t.TempDir()
	certPath, keyPath, err := GenerateServerCert(caCertPath, caKeyPath, serverDir, nil)
	if err != nil {
		t.Fatalf("GenerateServerCert: %v", err)
	}

	// Without CA cert (no client auth).
	cfg, err := LoadServerTLS(certPath, keyPath, "")
	if err != nil {
		t.Fatalf("LoadServerTLS without CA: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want %d", cfg.MinVersion, tls.VersionTLS12)
	}
	if cfg.ClientCAs != nil {
		t.Error("ClientCAs should be nil when no CA cert provided")
	}
}
