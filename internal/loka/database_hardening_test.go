package loka

import (
	"strings"
	"testing"
)

func TestEncryptDecrypt_Roundtrip(t *testing.T) {
	oldKey := EncryptionKey
	defer func() { EncryptionKey = oldKey }()

	EncryptionKey = "test-key-roundtrip"
	plaintext := "super-secret-password"

	encrypted := EncryptPassword(plaintext)
	if encrypted == plaintext {
		t.Fatal("encrypted should differ from plaintext")
	}
	if !strings.HasPrefix(encrypted, "enc:") {
		t.Fatalf("encrypted should start with enc:, got %q", encrypted)
	}

	decrypted := DecryptPassword(encrypted)
	if decrypted != plaintext {
		t.Errorf("roundtrip failed: expected %q, got %q", plaintext, decrypted)
	}
}

func TestEncryptPassword_NoKey_ReturnsPlaintext(t *testing.T) {
	oldKey := EncryptionKey
	defer func() { EncryptionKey = oldKey }()

	EncryptionKey = ""
	result := EncryptPassword("password")
	if result != "password" {
		t.Errorf("expected plaintext when no key, got %q", result)
	}
}

func TestEncryptPassword_EmptyInput(t *testing.T) {
	oldKey := EncryptionKey
	defer func() { EncryptionKey = oldKey }()

	EncryptionKey = "some-key"
	result := EncryptPassword("")
	if result != "" {
		t.Errorf("expected empty string for empty input, got %q", result)
	}
}

func TestDecryptPassword_PlaintextPassthrough(t *testing.T) {
	oldKey := EncryptionKey
	defer func() { EncryptionKey = oldKey }()

	EncryptionKey = "some-key"
	result := DecryptPassword("not-encrypted")
	if result != "not-encrypted" {
		t.Errorf("plaintext should pass through, got %q", result)
	}
}

func TestDecryptPassword_InvalidBase64(t *testing.T) {
	oldKey := EncryptionKey
	defer func() { EncryptionKey = oldKey }()

	EncryptionKey = "some-key"
	result := DecryptPassword("enc:not-valid-base64!!!")
	// Should return the encrypted string as-is (can't decode).
	if result != "enc:not-valid-base64!!!" {
		t.Errorf("invalid base64 should return as-is, got %q", result)
	}
}

func TestDecryptPassword_TruncatedCiphertext(t *testing.T) {
	oldKey := EncryptionKey
	defer func() { EncryptionKey = oldKey }()

	EncryptionKey = "some-key"
	// Valid base64 but too short for nonce extraction.
	result := DecryptPassword("enc:AAAA")
	if result == "some-key" {
		t.Error("should not return the key itself")
	}
}

func TestDecryptPassword_WrongKey_ReturnsRedacted(t *testing.T) {
	oldKey := EncryptionKey
	defer func() { EncryptionKey = oldKey }()

	EncryptionKey = "key-one"
	encrypted := EncryptPassword("my-password")

	EncryptionKey = "key-two"
	result := DecryptPassword(encrypted)

	if result == "my-password" {
		t.Error("should NOT return plaintext with wrong key")
	}
	if result == encrypted {
		t.Error("should NOT return ciphertext (information leak)")
	}
	if result != "[encrypted — key mismatch]" {
		t.Errorf("expected redacted placeholder, got %q", result)
	}
}

func TestEncryptPassword_DifferentNonces(t *testing.T) {
	oldKey := EncryptionKey
	defer func() { EncryptionKey = oldKey }()

	EncryptionKey = "test-key"
	e1 := EncryptPassword("same-password")
	e2 := EncryptPassword("same-password")

	if e1 == e2 {
		t.Error("encrypting same password twice should produce different ciphertexts (random nonce)")
	}
}

func TestGenerateLoginRole_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		role := GenerateLoginRole("testdb")
		if seen[role] {
			t.Fatalf("duplicate role generated: %s", role)
		}
		seen[role] = true
	}
}

func TestGenerateLoginRole_SuffixLength(t *testing.T) {
	role := GenerateLoginRole("db")
	// Format: db_login_<16 hex chars> (8 bytes = 16 hex)
	parts := strings.Split(role, "_login_")
	if len(parts) != 2 {
		t.Fatalf("expected format db_login_<hex>, got %q", role)
	}
	if len(parts[1]) != 16 {
		t.Errorf("expected 16 hex chars suffix (8 bytes), got %d chars: %q", len(parts[1]), parts[1])
	}
}

func TestSanitizeIdentifier_AllowsValid(t *testing.T) {
	tests := []struct{ input, want string }{
		{"mydb", "mydb"},
		{"my_db_123", "my_db_123"},
		{"DB_UPPER", "DB_UPPER"},
	}
	for _, tt := range tests {
		got := SanitizeIdentifier(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeIdentifier(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSanitizeIdentifier_StripsDangerous(t *testing.T) {
	tests := []struct{ input, want string }{
		{"my-db", "mydb"},
		{"my.db", "mydb"},
		{"my;db", "mydb"},
		{"my'db", "mydb"},
		{`my"db`, "mydb"},
		{"my db", "mydb"},
	}
	for _, tt := range tests {
		got := SanitizeIdentifier(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeIdentifier(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSanitizePassword_EscapesQuotes(t *testing.T) {
	tests := []struct{ input, want string }{
		{"simple", "simple"},
		{"it's", "it''s"},
		{`it"s`, `it"s`}, // Only single quotes escaped.
	}
	for _, tt := range tests {
		got := SanitizePassword(tt.input)
		if got != tt.want {
			t.Errorf("SanitizePassword(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
