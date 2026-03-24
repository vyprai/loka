package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Store manages credentials in ~/.loka/credentials.yaml
type Store struct {
	path string
	mu   sync.Mutex
}

// Secret represents a stored credential.
type Secret struct {
	Name  string `yaml:"name"`
	Type  string `yaml:"type"`  // "env", "aws", "gcs"
	Value string `yaml:"value"` // plain value (encryption TODO)
	// AWS-specific
	AccessKey string `yaml:"access_key,omitempty"`
	SecretKey string `yaml:"secret_key,omitempty"`
	Region    string `yaml:"region,omitempty"`
}

type credentialsFile struct {
	Secrets []Secret `yaml:"secrets"`
}

// secretRefPattern matches ${secret.<name>} references.
var secretRefPattern = regexp.MustCompile(`\$\{secret\.([^}]+)\}`)

// NewStore creates a Store that reads/writes ~/.loka/credentials.yaml.
func NewStore() *Store {
	home, _ := os.UserHomeDir()
	return &Store{
		path: filepath.Join(home, ".loka", "credentials.yaml"),
	}
}

// Set adds or updates a secret in the store.
func (s *Store) Set(secret Secret) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	creds, err := s.load()
	if err != nil {
		creds = &credentialsFile{}
	}

	// Update existing or append.
	found := false
	for i, sec := range creds.Secrets {
		if sec.Name == secret.Name {
			creds.Secrets[i] = secret
			found = true
			break
		}
	}
	if !found {
		creds.Secrets = append(creds.Secrets, secret)
	}

	return s.save(creds)
}

// Get retrieves a secret by name.
func (s *Store) Get(name string) (*Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	creds, err := s.load()
	if err != nil {
		return nil, err
	}

	for _, sec := range creds.Secrets {
		if sec.Name == name {
			return &sec, nil
		}
	}
	return nil, fmt.Errorf("secret %q not found", name)
}

// List returns all secrets with values redacted.
func (s *Store) List() ([]Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	creds, err := s.load()
	if err != nil {
		return nil, err
	}

	// Return names and types only, not values.
	result := make([]Secret, len(creds.Secrets))
	for i, sec := range creds.Secrets {
		result[i] = Secret{
			Name:   sec.Name,
			Type:   sec.Type,
			Region: sec.Region,
		}
		if sec.AccessKey != "" {
			result[i].AccessKey = maskValue(sec.AccessKey)
		}
	}
	return result, nil
}

// Remove deletes a secret by name.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	creds, err := s.load()
	if err != nil {
		return err
	}

	found := false
	filtered := make([]Secret, 0, len(creds.Secrets))
	for _, sec := range creds.Secrets {
		if sec.Name == name {
			found = true
			continue
		}
		filtered = append(filtered, sec)
	}
	if !found {
		return fmt.Errorf("secret %q not found", name)
	}

	creds.Secrets = filtered
	return s.save(creds)
}

// Resolve takes a string and replaces all ${secret.<name>} patterns with actual values.
func (s *Store) Resolve(value string) (string, error) {
	if !strings.Contains(value, "${secret.") {
		return value, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	creds, err := s.load()
	if err != nil {
		return value, err
	}

	// Build lookup map.
	lookup := make(map[string]Secret)
	for _, sec := range creds.Secrets {
		lookup[sec.Name] = sec
	}

	var resolveErr error
	result := secretRefPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := secretRefPattern.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		name := parts[1]

		// Support ${secret.name.field} syntax for AWS secrets.
		nameParts := strings.SplitN(name, ".", 2)
		secretName := nameParts[0]

		sec, ok := lookup[secretName]
		if !ok {
			resolveErr = fmt.Errorf("secret %q not found", secretName)
			return match
		}

		if len(nameParts) == 2 {
			field := nameParts[1]
			switch field {
			case "access_key":
				return sec.AccessKey
			case "secret_key":
				return sec.SecretKey
			case "region":
				return sec.Region
			case "value":
				return sec.Value
			default:
				resolveErr = fmt.Errorf("unknown field %q for secret %q", field, secretName)
				return match
			}
		}

		return sec.Value
	})

	return result, resolveErr
}

func (s *Store) load() (*credentialsFile, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &credentialsFile{}, nil
		}
		return nil, fmt.Errorf("read credentials file: %w", err)
	}

	var creds credentialsFile
	if err := yaml.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parse credentials file: %w", err)
	}
	return &creds, nil
}

func (s *Store) save(creds *credentialsFile) error {
	// Ensure directory exists.
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}

	data, err := yaml.Marshal(creds)
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}

	// Write with restrictive permissions.
	if err := os.WriteFile(s.path, data, 0600); err != nil {
		return fmt.Errorf("write credentials file: %w", err)
	}
	return nil
}

// maskValue shows only the first 4 characters of a value.
func maskValue(v string) string {
	if len(v) <= 4 {
		return "****"
	}
	return v[:4] + "****"
}
