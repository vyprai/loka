package vm

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// OverlayManager manages copy-on-write filesystem layers for session isolation.
//
// On Linux with lokavm, this uses real overlayfs mounts.
// For development on macOS, this uses directory-based COW simulation:
//
//	/data/sessions/<session-id>/
//	  ├── base/           # Initial state (from snapshot or empty)
//	  ├── layers/
//	  │   ├── layer-0/    # First checkpoint layer
//	  │   ├── layer-1/    # Second checkpoint layer
//	  │   └── ...
//	  └── workspace/      # Current working directory (merged view)
type OverlayManager struct {
	dataDir string
}

// NewOverlayManager creates a new overlay manager.
func NewOverlayManager(dataDir string) *OverlayManager {
	return &OverlayManager{dataDir: dataDir}
}

// SessionDir returns the root directory for a session's overlay data.
func (m *OverlayManager) SessionDir(sessionID string) string {
	return filepath.Join(m.dataDir, "sessions", sessionID)
}

// Init creates the directory structure for a new session.
func (m *OverlayManager) Init(sessionID string) error {
	dirs := []string{
		filepath.Join(m.SessionDir(sessionID), "base"),
		filepath.Join(m.SessionDir(sessionID), "layers"),
		filepath.Join(m.SessionDir(sessionID), "workspace"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", d, err)
		}
	}
	return nil
}

// WorkspacePath returns the current workspace path for a session.
func (m *OverlayManager) WorkspacePath(sessionID string) string {
	return filepath.Join(m.SessionDir(sessionID), "workspace")
}

// CreateLayer captures the current workspace diff as a new layer.
// Returns the layer name (used as checkpoint reference).
func (m *OverlayManager) CreateLayer(sessionID string) (string, error) {
	layerName := fmt.Sprintf("layer-%d", time.Now().UnixNano())
	layerDir := filepath.Join(m.SessionDir(sessionID), "layers", layerName)

	if err := os.MkdirAll(layerDir, 0o755); err != nil {
		return "", fmt.Errorf("create layer dir: %w", err)
	}

	// Clone workspace state into the layer using CoW when available.
	// macOS APFS: instant copy-on-write via cp -c (zero disk until writes).
	// Linux btrfs: reflink via cp --reflink=auto.
	// Fallback: regular recursive copy.
	workspace := m.WorkspacePath(sessionID)
	if err := cloneDir(workspace, layerDir); err != nil {
		return "", fmt.Errorf("snapshot workspace: %w", err)
	}

	return layerName, nil
}

// RestoreLayer restores the workspace to the state captured in a layer.
func (m *OverlayManager) RestoreLayer(sessionID, layerName string) error {
	layerDir := filepath.Join(m.SessionDir(sessionID), "layers", layerName)
	if _, err := os.Stat(layerDir); os.IsNotExist(err) {
		return fmt.Errorf("layer %s not found", layerName)
	}

	workspace := m.WorkspacePath(sessionID)

	// Clear current workspace.
	if err := os.RemoveAll(workspace); err != nil {
		return fmt.Errorf("clear workspace: %w", err)
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return fmt.Errorf("recreate workspace: %w", err)
	}

	// Clone layer state into workspace (CoW when available).
	if err := cloneDir(layerDir, workspace); err != nil {
		return fmt.Errorf("restore layer: %w", err)
	}

	return nil
}

// TarLayer creates a compressed tar archive of a layer.
func (m *OverlayManager) TarLayer(sessionID, layerName string, w io.Writer) error {
	layerDir := filepath.Join(m.SessionDir(sessionID), "layers", layerName)
	return tarDir(layerDir, w)
}

// TarWorkspace creates a compressed tar archive of the current workspace.
func (m *OverlayManager) TarWorkspace(sessionID string, w io.Writer) error {
	workspace := m.WorkspacePath(sessionID)
	return tarDir(workspace, w)
}

// UntarLayer extracts a tar archive into a new layer.
func (m *OverlayManager) UntarLayer(sessionID, layerName string, r io.Reader) error {
	layerDir := filepath.Join(m.SessionDir(sessionID), "layers", layerName)
	if err := os.MkdirAll(layerDir, 0o755); err != nil {
		return fmt.Errorf("create layer dir: %w", err)
	}
	return untarDir(layerDir, r)
}

// ListLayers returns all layers for a session, sorted by creation time.
func (m *OverlayManager) ListLayers(sessionID string) ([]string, error) {
	layersDir := filepath.Join(m.SessionDir(sessionID), "layers")
	entries, err := os.ReadDir(layersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var layers []string
	for _, e := range entries {
		if e.IsDir() {
			layers = append(layers, e.Name())
		}
	}
	sort.Strings(layers)
	return layers, nil
}

// DeleteLayer removes a layer.
func (m *OverlayManager) DeleteLayer(sessionID, layerName string) error {
	layerDir := filepath.Join(m.SessionDir(sessionID), "layers", layerName)
	return os.RemoveAll(layerDir)
}

// Cleanup removes all data for a session.
func (m *OverlayManager) Cleanup(sessionID string) error {
	return os.RemoveAll(m.SessionDir(sessionID))
}

// DiffLayers returns the list of files that differ between two layers.
// This is the simple API that returns just the entries.
func (m *OverlayManager) DiffLayers(sessionID, layerA, layerB string) ([]DiffEntry, error) {
	summary, err := m.FullDiff(sessionID, layerA, layerB)
	if err != nil {
		return nil, err
	}
	return summary.Entries, nil
}

// DiffType describes the type of change.
type DiffType string

const (
	DiffAdded    DiffType = "added"
	DiffModified DiffType = "modified"
	DiffDeleted  DiffType = "deleted"
	DiffModeChanged DiffType = "mode_changed" // Permissions changed.
)

// DiffEntry describes a single file difference between two snapshots.
type DiffEntry struct {
	Path     string   `json:"path"`
	Type     DiffType `json:"type"`
	Size     int64    `json:"size,omitempty"`      // Size in snapshot B (0 for deleted).
	OldSize  int64    `json:"old_size,omitempty"`   // Size in snapshot A.
	Hash     string   `json:"hash,omitempty"`       // Content hash in B.
	OldHash  string   `json:"old_hash,omitempty"`   // Content hash in A.
	Mode     string   `json:"mode,omitempty"`       // File mode in B.
	OldMode  string   `json:"old_mode,omitempty"`   // File mode in A.
	IsDir    bool     `json:"is_dir,omitempty"`
}

// DiffSummary is the aggregate summary of a diff.
type DiffSummary struct {
	Added       int   `json:"added"`
	Modified    int   `json:"modified"`
	Deleted     int   `json:"deleted"`
	ModeChanged int   `json:"mode_changed"`
	TotalFiles  int   `json:"total_files"`     // Total files in B.
	AddedBytes  int64 `json:"added_bytes"`
	DeletedBytes int64 `json:"deleted_bytes"`
	Entries     []DiffEntry `json:"entries"`
}

type fileInfo struct {
	Path string
	Size int64
	Hash string
	Mode os.FileMode
	IsDir bool
}

func listFiles(root string) ([]fileInfo, error) {
	var files []fileInfo
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		fi := fileInfo{
			Path:  rel,
			Size:  info.Size(),
			Mode:  info.Mode(),
			IsDir: info.IsDir(),
		}
		if !info.IsDir() {
			fi.Hash = hashFile(path)
		}
		files = append(files, fi)
		return nil
	})
	return files, err
}

func hashFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return fmt.Sprintf("%x", h.Sum(nil))[:16] // Short hash for comparison.
}

// ── Full Snapshot Diff ──────────────────────────────────

// FullDiff produces a detailed diff between two layer directories,
// including content hashing, permission changes, and a summary.
func (m *OverlayManager) FullDiff(sessionID, layerA, layerB string) (*DiffSummary, error) {
	dirA := filepath.Join(m.SessionDir(sessionID), "layers", layerA)
	dirB := filepath.Join(m.SessionDir(sessionID), "layers", layerB)
	return DiffDirs(dirA, dirB)
}

// DiffDirs computes a full diff between any two directories.
// Can be used across sessions or for arbitrary directory comparison.
func DiffDirs(dirA, dirB string) (*DiffSummary, error) {
	filesA, err := listFiles(dirA)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", dirA, err)
	}
	filesB, err := listFiles(dirB)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", dirB, err)
	}

	mapA := make(map[string]fileInfo)
	for _, f := range filesA {
		mapA[f.Path] = f
	}
	mapB := make(map[string]fileInfo)
	for _, f := range filesB {
		mapB[f.Path] = f
	}

	summary := &DiffSummary{TotalFiles: len(filesB)}

	// Added and modified.
	for path, fb := range mapB {
		fa, existed := mapA[path]
		if !existed {
			summary.Entries = append(summary.Entries, DiffEntry{
				Path: path, Type: DiffAdded, Size: fb.Size,
				Hash: fb.Hash, Mode: fb.Mode.String(), IsDir: fb.IsDir,
			})
			summary.Added++
			summary.AddedBytes += fb.Size
		} else {
			// Existed — check for content or mode changes.
			contentChanged := false
			modeChanged := false

			if !fb.IsDir && fa.Hash != fb.Hash {
				contentChanged = true
			}
			if fa.Mode != fb.Mode {
				modeChanged = true
			}

			if contentChanged {
				summary.Entries = append(summary.Entries, DiffEntry{
					Path: path, Type: DiffModified,
					Size: fb.Size, OldSize: fa.Size,
					Hash: fb.Hash, OldHash: fa.Hash,
					Mode: fb.Mode.String(), OldMode: fa.Mode.String(),
					IsDir: fb.IsDir,
				})
				summary.Modified++
			} else if modeChanged {
				summary.Entries = append(summary.Entries, DiffEntry{
					Path: path, Type: DiffModeChanged,
					Size: fb.Size,
					Mode: fb.Mode.String(), OldMode: fa.Mode.String(),
				})
				summary.ModeChanged++
			}
		}
	}

	// Deleted.
	for path, fa := range mapA {
		if _, ok := mapB[path]; !ok {
			summary.Entries = append(summary.Entries, DiffEntry{
				Path: path, Type: DiffDeleted, OldSize: fa.Size,
				OldHash: fa.Hash, OldMode: fa.Mode.String(), IsDir: fa.IsDir,
			})
			summary.Deleted++
			summary.DeletedBytes += fa.Size
		}
	}

	sort.Slice(summary.Entries, func(i, j int) bool {
		return summary.Entries[i].Path < summary.Entries[j].Path
	})

	return summary, nil
}

// ── Helpers ─────────────────────────────────────────────

// cloneDir creates a copy of src directory at dst, using CoW when available.
// macOS APFS: cp -c (instant clone, zero disk until writes).
// Linux: cp --reflink=auto (btrfs/XFS reflink, falls back to regular copy).
// Fallback: regular recursive copy.
func cloneDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create clone parent: %w", err)
	}
	if runtime.GOOS == "darwin" {
		// APFS clone: instant CoW.
		if err := exec.Command("cp", "-ac", src+"/.", dst).Run(); err == nil {
			return nil
		}
	} else {
		// Linux: try reflink first (btrfs/XFS).
		if err := exec.Command("cp", "-a", "--reflink=auto", src+"/.", dst).Run(); err == nil {
			return nil
		}
	}
	// Fallback: regular recursive copy.
	return copyDir(src, dst)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}

		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func tarDir(srcDir string, w io.Writer) error {
	gw := gzip.NewWriter(w)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(srcDir, path)
		if rel == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

func untarDir(dstDir string, r io.Reader) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(dstDir, header.Name)

		// Prevent path traversal.
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dstDir)) {
			return fmt.Errorf("invalid tar path: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}
