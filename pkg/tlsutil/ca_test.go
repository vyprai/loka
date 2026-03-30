package tlsutil

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestCertExpiryDetection(t *testing.T) {
	// Generate a CA and server cert, then verify we can detect expiry.
	caDir := t.TempDir()
	caCertPath, caKeyPath, err := GenerateCA(caDir)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	serverDir := t.TempDir()
	certPath, _, err := GenerateServerCert(caCertPath, caKeyPath, serverDir, nil)
	if err != nil {
		t.Fatalf("GenerateServerCert: %v", err)
	}

	// Read the generated cert and check its NotAfter.
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to decode cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	// Server cert should expire in ~365 days. Verify time.Until works.
	remaining := time.Until(cert.NotAfter)
	if remaining < 364*24*time.Hour || remaining > 366*24*time.Hour {
		t.Errorf("cert expiry remaining = %v, expected ~365 days", remaining)
	}

	// Verify that a cert expiring in > 30 days would NOT trigger renewal.
	if remaining < 30*24*time.Hour {
		t.Error("fresh cert should not be within 30-day renewal window")
	}

	// Verify certStillValid returns true for valid cert.
	if !certStillValid(certPath) {
		t.Error("certStillValid returned false for valid cert")
	}

	// Verify certStillValid returns false for non-existent file.
	if certStillValid("/nonexistent/cert.pem") {
		t.Error("certStillValid returned true for non-existent file")
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

func TestCertExpiry_ReturnsExpiry(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := GenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	expiry := CertExpiry(certPath)
	if expiry.IsZero() {
		t.Fatal("expected non-zero expiry")
	}

	// CA is valid for 5 years.
	expectedMin := time.Now().Add(4 * 365 * 24 * time.Hour)
	if expiry.Before(expectedMin) {
		t.Errorf("expiry %v is too soon (expected >4 years from now)", expiry)
	}
	expectedMax := time.Now().Add(6 * 365 * 24 * time.Hour)
	if expiry.After(expectedMax) {
		t.Errorf("expiry %v is too far (expected <6 years from now)", expiry)
	}
}

func TestCertExpiry_NonexistentFile(t *testing.T) {
	expiry := CertExpiry("/nonexistent/cert.pem")
	if !expiry.IsZero() {
		t.Errorf("expected zero time for nonexistent file, got %v", expiry)
	}
}

func TestCertExpiry_InvalidPEM(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "garbage.crt")
	if err := os.WriteFile(badPath, []byte("not valid PEM data"), 0o644); err != nil {
		t.Fatal(err)
	}

	expiry := CertExpiry(badPath)
	if !expiry.IsZero() {
		t.Errorf("expected zero time for invalid PEM, got %v", expiry)
	}
}

func TestCertStillValid_RegeneratesNearExpiry(t *testing.T) {
	dir := t.TempDir()

	// Generate a valid CA first.
	certPath, _, err := GenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Read original cert to get its serial number.
	origPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	origBlock, _ := pem.Decode(origPEM)
	if origBlock == nil {
		t.Fatal("failed to decode original cert PEM")
	}
	origCert, err := x509.ParseCertificate(origBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	origSerial := origCert.SerialNumber

	// Read the existing key to create a near-expiry cert.
	keyPath := filepath.Join(dir, "ca.key")
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		t.Fatal("failed to decode key PEM")
	}
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	// Create a cert that expires in 10 days (within the 30-day renewal window).
	// certStillValid checks: now + 30d < NotAfter. A cert expiring in 10d fails this.
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	nearExpiryTemplate := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "LOKA Auto CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &nearExpiryTemplate, &nearExpiryTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create near-expiry cert: %v", err)
	}

	// Overwrite the cert file with the near-expiry cert.
	if err := writePEM(certPath, "CERTIFICATE", certDER, 0o644); err != nil {
		t.Fatalf("write near-expiry cert: %v", err)
	}

	// Verify certStillValid returns false for the near-expiry cert.
	if certStillValid(certPath) {
		t.Fatal("certStillValid should return false for cert expiring in 10 days")
	}

	// Call GenerateCA again — it should regenerate because the cert is near expiry.
	certPath2, _, err := GenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	newPEM, err := os.ReadFile(certPath2)
	if err != nil {
		t.Fatal(err)
	}
	newBlock, _ := pem.Decode(newPEM)
	if newBlock == nil {
		t.Fatal("failed to decode regenerated cert PEM")
	}
	newCert, err := x509.ParseCertificate(newBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	// The regenerated cert should have a different serial number.
	if origSerial.Cmp(newCert.SerialNumber) == 0 {
		t.Error("expected different serial after regeneration, got same serial")
	}

	// The regenerated cert should expire ~5 years from now, not 10 days.
	minExpiry := time.Now().Add(4 * 365 * 24 * time.Hour)
	if newCert.NotAfter.Before(minExpiry) {
		t.Errorf("regenerated cert NotAfter %v is too soon, expected >4 years", newCert.NotAfter)
	}
}
