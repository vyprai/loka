package registry

import (
	"fmt"
	"strings"

	"github.com/vyprai/loka/internal/secret"
)

// AuthConfig holds authentication credentials for a remote registry.
type AuthConfig struct {
	Token    string // Bearer token
	Username string // Basic auth
	Password string
}

// ResolveAuth resolves authentication for a registry configuration.
// It handles ${secret.xxx} references via the secret store.
func ResolveAuth(reg RegistryConfig, secretStore *secret.Store) (*AuthConfig, error) {
	auth := &AuthConfig{}

	// Resolve token.
	if reg.Token != "" {
		if secretStore != nil && strings.Contains(reg.Token, "${secret.") {
			resolved, err := secretStore.Resolve(reg.Token)
			if err != nil {
				return nil, fmt.Errorf("resolve token secret: %w", err)
			}
			auth.Token = resolved
		} else {
			auth.Token = reg.Token
		}
	}

	// Resolve credentials (username:password or secret reference).
	if reg.Credentials != "" {
		cred := reg.Credentials
		if secretStore != nil && strings.Contains(cred, "${secret.") {
			resolved, err := secretStore.Resolve(cred)
			if err != nil {
				return nil, fmt.Errorf("resolve credentials secret: %w", err)
			}
			cred = resolved
		}
		// Parse username:password format.
		if parts := strings.SplitN(cred, ":", 2); len(parts) == 2 {
			auth.Username = parts[0]
			auth.Password = parts[1]
		} else {
			// Treat as token if no colon.
			auth.Token = cred
		}
	}

	return auth, nil
}
