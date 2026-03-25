package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/worker/vm"
)

// FileProxy handles host-side file operations for FUSE volume mounts.
// When the supervisor inside the VM needs to read/write files, it sends
// RPCs to the host via vsock. The FileProxy translates these to objstore calls.
type FileProxy struct {
	store objstore.ObjectStore
}

// NewFileProxy creates a file proxy backed by the given object store.
func NewFileProxy(store objstore.ObjectStore) *FileProxy {
	return &FileProxy{store: store}
}

// HandleFsStat checks if an object exists and returns its metadata.
func (fp *FileProxy) HandleFsStat(ctx context.Context, req vm.FsStatRequest) (*vm.FsStatResult, error) {
	if req.Key == "" || strings.HasSuffix(req.Key, "/") {
		// Directory-like prefix: check if any objects exist under it.
		prefix := req.Key
		if !strings.HasSuffix(prefix, "/") && prefix != "" {
			prefix += "/"
		}
		entries, err := fp.store.List(ctx, req.Bucket, prefix)
		if err != nil {
			return &vm.FsStatResult{Exists: false}, nil
		}
		return &vm.FsStatResult{
			Exists: len(entries) > 0,
			IsDir:  true,
		}, nil
	}

	exists, err := fp.store.Exists(ctx, req.Bucket, req.Key)
	if err != nil {
		return nil, fmt.Errorf("check existence: %w", err)
	}
	if !exists {
		return &vm.FsStatResult{Exists: false}, nil
	}

	// Get the object to determine size.
	reader, err := fp.store.Get(ctx, req.Bucket, req.Key)
	if err != nil {
		return &vm.FsStatResult{Exists: true}, nil
	}
	data, _ := io.ReadAll(reader)
	reader.Close()

	return &vm.FsStatResult{
		Exists: true,
		Size:   int64(len(data)),
	}, nil
}

// HandleFsRead reads an object from the store and returns its content base64-encoded.
func (fp *FileProxy) HandleFsRead(ctx context.Context, req vm.FsReadRequest) (*vm.FsReadResult, error) {
	reader, err := fp.store.Get(ctx, req.Bucket, req.Key)
	if err != nil {
		return nil, fmt.Errorf("get object %s/%s: %w", req.Bucket, req.Key, err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read object: %w", err)
	}

	// If offset and length are specified, return a slice.
	if req.Offset > 0 || req.Length > 0 {
		start := int(req.Offset)
		if start > len(data) {
			start = len(data)
		}
		end := len(data)
		if req.Length > 0 && start+req.Length < end {
			end = start + req.Length
		}
		data = data[start:end]
	}

	return &vm.FsReadResult{
		Data: base64.StdEncoding.EncodeToString(data),
		Size: int64(len(data)),
	}, nil
}

// HandleFsWrite writes data to the object store.
func (fp *FileProxy) HandleFsWrite(ctx context.Context, req vm.FsWriteRequest) error {
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		return fmt.Errorf("decode base64: %w", err)
	}

	return fp.store.Put(ctx, req.Bucket, req.Key, bytes.NewReader(data), int64(len(data)))
}

// HandleFsList lists objects under a prefix and returns directory entries.
func (fp *FileProxy) HandleFsList(ctx context.Context, req vm.FsListRequest) ([]vm.FsListEntry, error) {
	objects, err := fp.store.List(ctx, req.Bucket, req.Prefix)
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}

	// Deduplicate directories and convert to entries.
	seen := make(map[string]bool)
	var entries []vm.FsListEntry

	for _, obj := range objects {
		// Strip the prefix to get relative path.
		rel := strings.TrimPrefix(obj.Key, req.Prefix)
		rel = strings.TrimPrefix(rel, "/")

		if rel == "" {
			continue
		}

		// If the relative path contains a slash, the first component is a directory.
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) > 1 {
			dirName := parts[0]
			fullDir := filepath.Join(req.Prefix, dirName)
			if !seen[fullDir] {
				seen[fullDir] = true
				entries = append(entries, vm.FsListEntry{
					Name:  fullDir,
					IsDir: true,
				})
			}
		}

		entries = append(entries, vm.FsListEntry{
			Name: obj.Key,
			Size: obj.Size,
		})
	}

	return entries, nil
}

// HandleFsDelete removes an object from the store.
func (fp *FileProxy) HandleFsDelete(ctx context.Context, req vm.FsDeleteRequest) error {
	return fp.store.Delete(ctx, req.Bucket, req.Key)
}

// HandleFileRPC dispatches a file-related RPC method to the appropriate handler.
// Returns the JSON result or an error.
func (fp *FileProxy) HandleFileRPC(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "fs_stat":
		var req vm.FsStatRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		result, err := fp.HandleFsStat(ctx, req)
		if err != nil {
			return nil, err
		}
		data, _ := json.Marshal(result)
		return data, nil

	case "fs_read":
		var req vm.FsReadRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		result, err := fp.HandleFsRead(ctx, req)
		if err != nil {
			return nil, err
		}
		data, _ := json.Marshal(result)
		return data, nil

	case "fs_write":
		var req vm.FsWriteRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if err := fp.HandleFsWrite(ctx, req); err != nil {
			return nil, err
		}
		data, _ := json.Marshal(map[string]bool{"ok": true})
		return data, nil

	case "fs_list":
		var req vm.FsListRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		result, err := fp.HandleFsList(ctx, req)
		if err != nil {
			return nil, err
		}
		data, _ := json.Marshal(result)
		return data, nil

	case "fs_delete":
		var req vm.FsDeleteRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if err := fp.HandleFsDelete(ctx, req); err != nil {
			return nil, err
		}
		data, _ := json.Marshal(map[string]bool{"ok": true})
		return data, nil

	default:
		return nil, fmt.Errorf("unknown file proxy method: %s", method)
	}
}
