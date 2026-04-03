package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryption_SetGet_Roundtrip(t *testing.T) {
	s := &Store{path: filepath.Join(t.TempDir(), "creds.yaml")}
	EncryptionKey = "test-roundtrip-key"
	defer func() { EncryptionKey = "" }()

	require.NoError(t, s.Set(Secret{Name: "db", Type: "env", Value: "super-secret"}))

	got, err := s.Get("db")
	require.NoError(t, err)
	assert.Equal(t, "super-secret", got.Value)
}

func TestEncryption_ValueNotInFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.yaml")
	s := &Store{path: path}
	EncryptionKey = "file-check-key"
	defer func() { EncryptionKey = "" }()

	s.Set(Secret{Name: "db", Type: "env", Value: "plaintext-leak-check"})

	data, _ := os.ReadFile(path)
	assert.NotContains(t, string(data), "plaintext-leak-check",
		"plaintext value should NOT appear in credentials file")
	assert.Contains(t, string(data), "enc:",
		"encrypted value should have enc: prefix")
}

func TestEncryption_AWSSecretKeyEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.yaml")
	s := &Store{path: path}
	EncryptionKey = "aws-enc-key"
	defer func() { EncryptionKey = "" }()

	s.Set(Secret{
		Name: "aws", Type: "aws",
		AccessKey: "AKIA123", SecretKey: "my-aws-secret", Region: "us-east-1",
	})

	data, _ := os.ReadFile(path)
	assert.NotContains(t, string(data), "my-aws-secret")

	got, _ := s.Get("aws")
	assert.Equal(t, "my-aws-secret", got.SecretKey)
	assert.Equal(t, "AKIA123", got.AccessKey) // AccessKey not encrypted.
}

func TestEncryption_WrongKey_ReturnsRedacted(t *testing.T) {
	s := &Store{path: filepath.Join(t.TempDir(), "creds.yaml")}
	EncryptionKey = "key-one"
	s.Set(Secret{Name: "sec", Type: "env", Value: "data"})

	EncryptionKey = "key-two"
	got, _ := s.Get("sec")
	assert.NotEqual(t, "data", got.Value)
	assert.Equal(t, "[encrypted — key mismatch]", got.Value)
	EncryptionKey = ""
}

func TestEncryption_KeyChange_OldDataUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.yaml")
	s := &Store{path: path}

	EncryptionKey = "old-key"
	s.Set(Secret{Name: "old", Type: "env", Value: "old-data"})

	EncryptionKey = "new-key"
	got, _ := s.Get("old")
	assert.Equal(t, "[encrypted — key mismatch]", got.Value)
	EncryptionKey = ""
}

func TestEncryption_Resolve_DecryptsBeforeResolving(t *testing.T) {
	s := &Store{path: filepath.Join(t.TempDir(), "creds.yaml")}
	EncryptionKey = "resolve-enc-key"
	defer func() { EncryptionKey = "" }()

	s.Set(Secret{Name: "pass", Type: "env", Value: "s3cr3t"})

	resolved, err := s.Resolve("pw=${secret.pass}")
	require.NoError(t, err)
	assert.Equal(t, "pw=s3cr3t", resolved)
}

func TestEncryption_Resolve_AWSFieldDecrypted(t *testing.T) {
	s := &Store{path: filepath.Join(t.TempDir(), "creds.yaml")}
	EncryptionKey = "aws-resolve-key"
	defer func() { EncryptionKey = "" }()

	s.Set(Secret{Name: "aws", Type: "aws", SecretKey: "sk123", AccessKey: "ak", Region: "eu"})

	resolved, err := s.Resolve("${secret.aws.secret_key}")
	require.NoError(t, err)
	assert.Equal(t, "sk123", resolved)
}

func TestEncryption_EmptyValues_Passthrough(t *testing.T) {
	EncryptionKey = "some-key"
	defer func() { EncryptionKey = "" }()

	assert.Equal(t, "", encryptValue(""))
	assert.Equal(t, "", decryptValue(""))
}

func TestEncryption_NoKey_Passthrough(t *testing.T) {
	EncryptionKey = ""
	assert.Equal(t, "data", encryptValue("data"))
	assert.Equal(t, "data", decryptValue("data"))
}

func TestDecryption_InvalidBase64(t *testing.T) {
	EncryptionKey = "key"
	defer func() { EncryptionKey = "" }()

	// Invalid base64 should return as-is.
	result := decryptValue("enc:not-valid-base64!!!")
	assert.Equal(t, "enc:not-valid-base64!!!", result)
}

func TestDecryption_TruncatedCiphertext(t *testing.T) {
	EncryptionKey = "key"
	defer func() { EncryptionKey = "" }()

	result := decryptValue("enc:AAAA")
	assert.NotEmpty(t, result)
}

func TestEncryption_DifferentNonces(t *testing.T) {
	EncryptionKey = "nonce-test"
	defer func() { EncryptionKey = "" }()

	e1 := encryptValue("same-data")
	e2 := encryptValue("same-data")
	assert.NotEqual(t, e1, e2, "same plaintext should produce different ciphertexts")
}

func TestEncryption_Persistence_AcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.yaml")
	EncryptionKey = "persist-key"
	defer func() { EncryptionKey = "" }()

	s1 := &Store{path: path}
	s1.Set(Secret{Name: "persist", Type: "env", Value: "hello"})

	s2 := &Store{path: path}
	got, err := s2.Get("persist")
	require.NoError(t, err)
	assert.Equal(t, "hello", got.Value)
}

func TestResolve_UnknownField_ReturnsError(t *testing.T) {
	s := &Store{path: filepath.Join(t.TempDir(), "creds.yaml")}
	EncryptionKey = ""
	s.Set(Secret{Name: "x", Type: "env", Value: "v"})

	_, err := s.Resolve("${secret.x.badfield}")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}

func TestMaskValue(t *testing.T) {
	assert.Equal(t, "****", maskValue("abc"))
	assert.Equal(t, "****", maskValue("abcd"))
	assert.Equal(t, "AKIA****", maskValue("AKIA12345"))
}

func TestListRedactsAll(t *testing.T) {
	s := &Store{path: filepath.Join(t.TempDir(), "creds.yaml")}
	EncryptionKey = ""

	s.Set(Secret{Name: "a", Type: "env", Value: "secret-val"})
	list, _ := s.List()

	for _, sec := range list {
		assert.Empty(t, sec.Value, "Value should be redacted")
		assert.Empty(t, sec.SecretKey, "SecretKey should be redacted")
	}
}

func TestEncryption_LargeValue(t *testing.T) {
	EncryptionKey = "large-val-key"
	defer func() { EncryptionKey = "" }()

	large := strings.Repeat("x", 100000)
	encrypted := encryptValue(large)
	decrypted := decryptValue(encrypted)
	assert.Equal(t, large, decrypted)
}

func TestEncryption_SpecialCharacters(t *testing.T) {
	EncryptionKey = "special-chars"
	defer func() { EncryptionKey = "" }()

	special := "p@$$w0rd!#%^&*()_+-={}[]|;':\",./<>?"
	encrypted := encryptValue(special)
	decrypted := decryptValue(encrypted)
	assert.Equal(t, special, decrypted)
}

func TestEncryption_Unicode(t *testing.T) {
	EncryptionKey = "unicode-key"
	defer func() { EncryptionKey = "" }()

	unicode := "密码🔑パスワード"
	encrypted := encryptValue(unicode)
	decrypted := decryptValue(encrypted)
	assert.Equal(t, unicode, decrypted)
}
