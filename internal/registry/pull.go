package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
)

// Pull fetches an image from a remote registry into the local store.
// It downloads the manifest, config blob, and all layer blobs, deduplicating
// blobs that already exist in the store.
func Pull(ctx context.Context, ref string, store *Store, auth *AuthConfig) (*OCIManifest, error) {
	reg, repo, tag := ParseReference(ref)

	// Get auth token.
	token, err := getToken(ctx, reg, repo, auth)
	if err != nil {
		return nil, fmt.Errorf("auth for %s/%s: %w", reg, repo, err)
	}

	// Fetch manifest — might be a manifest list (multi-arch) or single manifest.
	manifest, err := fetchManifest(ctx, reg, repo, tag, token)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest %s/%s:%s: %w", reg, repo, tag, err)
	}

	// Download config blob.
	if !store.HasBlob(ctx, manifest.Config.Digest) {
		reader, err := fetchBlob(ctx, reg, repo, manifest.Config.Digest, token)
		if err != nil {
			return nil, fmt.Errorf("fetch config blob: %w", err)
		}
		if err := store.PutBlob(ctx, manifest.Config.Digest, reader, manifest.Config.Size); err != nil {
			reader.Close()
			return nil, fmt.Errorf("store config blob: %w", err)
		}
		reader.Close()
	}

	// Download layers (skip if already have digest).
	for _, layer := range manifest.Layers {
		if store.HasBlob(ctx, layer.Digest) {
			continue
		}
		reader, err := fetchBlob(ctx, reg, repo, layer.Digest, token)
		if err != nil {
			return nil, fmt.Errorf("fetch layer %s: %w", layer.Digest, err)
		}
		if err := store.PutBlob(ctx, layer.Digest, reader, layer.Size); err != nil {
			reader.Close()
			return nil, fmt.Errorf("store layer %s: %w", layer.Digest, err)
		}
		reader.Close()
	}

	// Store manifest locally.
	if err := store.PutManifest(ctx, repo, tag, manifest); err != nil {
		return nil, fmt.Errorf("store manifest: %w", err)
	}

	return manifest, nil
}

// ParseReference parses a container image reference into registry, repository, and tag.
//
//	node:20-slim → (registry-1.docker.io, library/node, 20-slim)
//	docker.io/library/node:20-slim → (registry-1.docker.io, library/node, 20-slim)
//	ghcr.io/org/app:latest → (ghcr.io, org/app, latest)
func ParseReference(ref string) (registry, repo, tag string) {
	// Default tag.
	tag = "latest"

	// Split off tag or digest.
	if idx := strings.LastIndex(ref, ":"); idx > 0 {
		// Check it's not part of the registry (e.g., localhost:5000/repo).
		afterColon := ref[idx+1:]
		if !strings.Contains(afterColon, "/") {
			tag = afterColon
			ref = ref[:idx]
		}
	}

	// Split registry from repo.
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 1 {
		// Short name like "node" or "ubuntu".
		registry = "registry-1.docker.io"
		repo = "library/" + parts[0]
		return
	}

	// Check if first part looks like a registry (contains dot or colon or is "localhost").
	first := parts[0]
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		registry = first
		repo = parts[1]

		// Normalize Docker Hub registry names.
		if registry == "docker.io" || registry == "index.docker.io" {
			registry = "registry-1.docker.io"
		}

		// Docker Hub: add library/ prefix for official images.
		if registry == "registry-1.docker.io" && !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
		return
	}

	// No registry — Docker Hub.
	registry = "registry-1.docker.io"
	repo = ref
	return
}

// getToken obtains a bearer token for the given registry and repository.
func getToken(ctx context.Context, registry, repo string, auth *AuthConfig) (string, error) {
	// If auth provides a token directly (e.g. GHCR), use it.
	if auth != nil && auth.Token != "" {
		return auth.Token, nil
	}

	// Docker Hub token auth.
	if registry == "registry-1.docker.io" {
		return getDockerHubToken(ctx, repo, auth)
	}

	// Other registries: try anonymous or basic auth.
	// Many registries support anonymous pulls. The token will be empty.
	if auth != nil && auth.Username != "" {
		// Basic auth — no token exchange needed.
		return "", nil
	}

	return "", nil
}

// getDockerHubToken fetches a token from Docker Hub's auth service.
func getDockerHubToken(ctx context.Context, repo string, auth *AuthConfig) (string, error) {
	url := fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", repo)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	if auth != nil && auth.Username != "" && auth.Password != "" {
		req.SetBasicAuth(auth.Username, auth.Password)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token request failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	return tokenResp.Token, nil
}

// fetchManifest retrieves the image manifest from a remote registry.
// If the manifest is a manifest list (multi-arch), it selects the manifest for the current platform.
func fetchManifest(ctx context.Context, registry, repo, tag, token string) (*OCIManifest, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, repo, tag)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	// Accept multiple manifest types.
	req.Header.Set("Accept", strings.Join([]string{
		MediaTypeDockerManifest,
		MediaTypeOCIManifest,
		MediaTypeDockerManifestList,
		MediaTypeOCIIndex,
	}, ", "))

	setAuthHeader(req, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch manifest (%d): %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read manifest body: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")

	// Handle manifest list / OCI index (multi-arch).
	if contentType == MediaTypeDockerManifestList || contentType == MediaTypeOCIIndex {
		return resolveManifestList(ctx, registry, repo, token, body)
	}

	// Single manifest.
	var manifest OCIManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	// Ensure media type is set.
	if manifest.MediaType == "" {
		manifest.MediaType = contentType
	}

	return &manifest, nil
}

// resolveManifestList selects the manifest for the current platform from a manifest list.
func resolveManifestList(ctx context.Context, registry, repo, token string, body []byte) (*OCIManifest, error) {
	var list ManifestList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode manifest list: %w", err)
	}

	targetArch := runtime.GOARCH
	targetOS := runtime.GOOS

	// Map Go arch names to OCI platform names.
	if targetArch == "arm64" {
		targetArch = "arm64"
	}

	// Find matching platform.
	var selected *PlatformManifest
	for i := range list.Manifests {
		m := &list.Manifests[i]
		if m.Platform.Architecture == targetArch && m.Platform.OS == targetOS {
			selected = m
			break
		}
	}

	if selected == nil {
		// Fall back to amd64/linux.
		for i := range list.Manifests {
			m := &list.Manifests[i]
			if m.Platform.Architecture == "amd64" && m.Platform.OS == "linux" {
				selected = m
				break
			}
		}
	}

	if selected == nil && len(list.Manifests) > 0 {
		selected = &list.Manifests[0]
	}

	if selected == nil {
		return nil, fmt.Errorf("no matching manifest found for %s/%s", targetOS, targetArch)
	}

	// Fetch the actual manifest by digest.
	return fetchManifest(ctx, registry, repo, selected.Digest, token)
}

// fetchBlob downloads a blob from a remote registry.
func fetchBlob(ctx context.Context, registry, repo, digest, token string) (io.ReadCloser, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", registry, repo, digest)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	setAuthHeader(req, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch blob %s: %w", digest, err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("fetch blob %s (%d): %s", digest, resp.StatusCode, string(body))
	}

	return resp.Body, nil
}

// setAuthHeader sets the Authorization header for a registry request.
func setAuthHeader(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}
