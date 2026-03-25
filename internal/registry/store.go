package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/vyprai/loka/internal/objstore"
)

const (
	registryBucket     = "loka"
	blobPrefix         = "registry/blobs/"
	manifestPrefix     = "registry/manifests/"
)

// Store provides storage for OCI blobs and manifests backed by an object store.
type Store struct {
	objStore objstore.ObjectStore
}

// NewStore creates a new registry store backed by the given object store.
func NewStore(objStore objstore.ObjectStore) *Store {
	return &Store{
		objStore: objStore,
	}
}

// ── Blobs ───────────────────────────────────────────────

// HasBlob checks if a blob exists in the store.
func (s *Store) HasBlob(ctx context.Context, digest string) bool {
	exists, _ := s.objStore.Exists(ctx, registryBucket, blobPrefix+digest)
	return exists
}

// GetBlob retrieves a blob by digest. The caller must close the returned reader.
func (s *Store) GetBlob(ctx context.Context, digest string) (io.ReadCloser, int64, error) {
	rc, err := s.objStore.Get(ctx, registryBucket, blobPrefix+digest)
	if err != nil {
		return nil, 0, fmt.Errorf("get blob %s: %w", digest, err)
	}

	// Get size from object info.
	objects, err := s.objStore.List(ctx, registryBucket, blobPrefix+digest)
	if err != nil || len(objects) == 0 {
		// Return reader without size if we can't determine it.
		return rc, -1, nil
	}
	return rc, objects[0].Size, nil
}

// PutBlob stores a blob by digest.
func (s *Store) PutBlob(ctx context.Context, digest string, reader io.Reader, size int64) error {
	return s.objStore.Put(ctx, registryBucket, blobPrefix+digest, reader, size)
}

// DeleteBlob removes a blob by digest.
func (s *Store) DeleteBlob(ctx context.Context, digest string) error {
	return s.objStore.Delete(ctx, registryBucket, blobPrefix+digest)
}

// ── Manifests ───────────────────────────────────────────

func manifestKey(name, reference string) string {
	return manifestPrefix + name + "/" + reference + ".json"
}

// GetManifest retrieves a manifest by repository name and reference (tag or digest).
func (s *Store) GetManifest(ctx context.Context, name, reference string) (*OCIManifest, error) {
	rc, err := s.objStore.Get(ctx, registryBucket, manifestKey(name, reference))
	if err != nil {
		return nil, fmt.Errorf("get manifest %s:%s: %w", name, reference, err)
	}
	defer rc.Close()

	var manifest OCIManifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode manifest %s:%s: %w", name, reference, err)
	}
	return &manifest, nil
}

// PutManifest stores a manifest for a repository name and reference.
func (s *Store) PutManifest(ctx context.Context, name, reference string, manifest *OCIManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	return s.objStore.Put(ctx, registryBucket, manifestKey(name, reference), bytes.NewReader(data), int64(len(data)))
}

// DeleteManifest removes a manifest for a repository name and reference.
func (s *Store) DeleteManifest(ctx context.Context, name, reference string) error {
	return s.objStore.Delete(ctx, registryBucket, manifestKey(name, reference))
}

// ListRepositories returns all repository names in the store.
func (s *Store) ListRepositories(ctx context.Context) ([]string, error) {
	objects, err := s.objStore.List(ctx, registryBucket, manifestPrefix)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}

	repoSet := make(map[string]struct{})
	for _, obj := range objects {
		// Key format: registry/manifests/{name}/{reference}.json
		// name may contain slashes (e.g., library/node).
		key := strings.TrimPrefix(obj.Key, manifestPrefix)
		// Find the last slash to separate name from reference.
		lastSlash := strings.LastIndex(key, "/")
		if lastSlash > 0 {
			repoSet[key[:lastSlash]] = struct{}{}
		}
	}

	repos := make([]string, 0, len(repoSet))
	for repo := range repoSet {
		repos = append(repos, repo)
	}
	return repos, nil
}

// ListTags returns all tags for a repository.
func (s *Store) ListTags(ctx context.Context, name string) ([]string, error) {
	objects, err := s.objStore.List(ctx, registryBucket, manifestPrefix+name+"/")
	if err != nil {
		return nil, fmt.Errorf("list tags for %s: %w", name, err)
	}

	var tags []string
	for _, obj := range objects {
		key := strings.TrimPrefix(obj.Key, manifestPrefix+name+"/")
		key = strings.TrimSuffix(key, ".json")
		if key != "" {
			tags = append(tags, key)
		}
	}
	return tags, nil
}

// ListBlobs returns all blob digests in the store.
func (s *Store) ListBlobs(ctx context.Context) ([]string, error) {
	objects, err := s.objStore.List(ctx, registryBucket, blobPrefix)
	if err != nil {
		return nil, fmt.Errorf("list blobs: %w", err)
	}

	var digests []string
	for _, obj := range objects {
		digest := strings.TrimPrefix(obj.Key, blobPrefix)
		if digest != "" {
			digests = append(digests, digest)
		}
	}
	return digests, nil
}
