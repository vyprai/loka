package secret

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	path := filepath.Join(t.TempDir(), "credentials.yaml")
	return &Store{path: path}
}

func TestSecretSetAndGet(t *testing.T) {
	s := newTestStore(t)

	err := s.Set(Secret{
		Name:  "mydb",
		Type:  "env",
		Value: "postgres://localhost:5432/mydb",
	})
	require.NoError(t, err)

	sec, err := s.Get("mydb")
	require.NoError(t, err)
	assert.Equal(t, "mydb", sec.Name)
	assert.Equal(t, "env", sec.Type)
	assert.Equal(t, "postgres://localhost:5432/mydb", sec.Value)
}

func TestSecretList(t *testing.T) {
	s := newTestStore(t)

	require.NoError(t, s.Set(Secret{Name: "db", Type: "env", Value: "postgres://..."}))
	require.NoError(t, s.Set(Secret{Name: "api_key", Type: "env", Value: "sk-123"}))
	require.NoError(t, s.Set(Secret{Name: "redis", Type: "env", Value: "redis://..."}))

	list, err := s.List()
	require.NoError(t, err)
	assert.Len(t, list, 3)

	names := make(map[string]bool)
	for _, sec := range list {
		names[sec.Name] = true
	}
	assert.True(t, names["db"])
	assert.True(t, names["api_key"])
	assert.True(t, names["redis"])
}

func TestSecretRemove(t *testing.T) {
	s := newTestStore(t)

	require.NoError(t, s.Set(Secret{Name: "temp", Type: "env", Value: "tempval"}))

	// Verify it exists.
	_, err := s.Get("temp")
	require.NoError(t, err)

	// Remove it.
	require.NoError(t, s.Remove("temp"))

	// Verify it's gone.
	_, err = s.Get("temp")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSecretRemoveNotFound(t *testing.T) {
	s := newTestStore(t)
	err := s.Remove("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSecretResolve(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.Set(Secret{Name: "mydb", Type: "env", Value: "postgres://localhost:5432/mydb"}))

	resolved, err := s.Resolve("${secret.mydb}")
	require.NoError(t, err)
	assert.Equal(t, "postgres://localhost:5432/mydb", resolved)
}

func TestSecretResolveMultiple(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.Set(Secret{Name: "host", Type: "env", Value: "localhost"}))
	require.NoError(t, s.Set(Secret{Name: "port", Type: "env", Value: "5432"}))

	resolved, err := s.Resolve("jdbc:postgresql://${secret.host}:${secret.port}/mydb")
	require.NoError(t, err)
	assert.Equal(t, "jdbc:postgresql://localhost:5432/mydb", resolved)
}

func TestSecretResolveNoRef(t *testing.T) {
	s := newTestStore(t)

	resolved, err := s.Resolve("plain string without secrets")
	require.NoError(t, err)
	assert.Equal(t, "plain string without secrets", resolved)
}

func TestSecretResolveMissing(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Resolve("${secret.nonexistent}")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSecretResolveAWSFields(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.Set(Secret{
		Name:      "myaws",
		Type:      "aws",
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:    "us-east-1",
	}))

	ak, err := s.Resolve("${secret.myaws.access_key}")
	require.NoError(t, err)
	assert.Equal(t, "AKIAIOSFODNN7EXAMPLE", ak)

	sk, err := s.Resolve("${secret.myaws.secret_key}")
	require.NoError(t, err)
	assert.Equal(t, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", sk)

	region, err := s.Resolve("${secret.myaws.region}")
	require.NoError(t, err)
	assert.Equal(t, "us-east-1", region)
}

func TestSecretPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.yaml")

	// First store instance: set a secret.
	s1 := &Store{path: path}
	require.NoError(t, s1.Set(Secret{Name: "persistent", Type: "env", Value: "hello"}))

	// Second store instance: verify the secret is still there.
	s2 := &Store{path: path}
	sec, err := s2.Get("persistent")
	require.NoError(t, err)
	assert.Equal(t, "persistent", sec.Name)
	assert.Equal(t, "hello", sec.Value)
}

func TestSecretUpdate(t *testing.T) {
	s := newTestStore(t)

	require.NoError(t, s.Set(Secret{Name: "key", Type: "env", Value: "old"}))
	require.NoError(t, s.Set(Secret{Name: "key", Type: "env", Value: "new"}))

	sec, err := s.Get("key")
	require.NoError(t, err)
	assert.Equal(t, "new", sec.Value)
}
