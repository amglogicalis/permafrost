package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/amglogicalis/permafrost/internal/config"
)

// ChunkRef represents a single content-defined block inside a file.
type ChunkRef struct {
	Hash           string `json:"hash"`
	Offset         int64  `json:"offset"`
	RawSize        int64  `json:"raw_size"`
	CompressedSize int64  `json:"compressed_size"`
}

// FileEntry describes a single tracked file or directory in a Snapshot Manifest.
type FileEntry struct {
	RelPath  string      `json:"rel_path"`
	Size     int64       `json:"size"`
	Mode     uint32      `json:"mode"`
	ModTime  time.Time   `json:"mod_time"`
	IsDir    bool        `json:"is_dir,omitempty"`
	FileHash string      `json:"file_hash,omitempty"`
	Chunks   []ChunkRef  `json:"chunks,omitempty"`
}

// SnapshotStats holds deduplication and compression metrics for a snapshot.
type SnapshotStats struct {
	TotalFiles          int     `json:"total_files"`
	LogicalBytes        int64   `json:"logical_bytes"`
	TotalChunks         int     `json:"total_chunks"`
	NewUniqueChunks     int     `json:"new_unique_chunks"`
	ReusedChunks        int     `json:"reused_chunks"`
	NewBytesStored      int64   `json:"new_bytes_stored"`
	TotalCompressedSize int64   `json:"total_compressed_size"`
	SpaceSavedPercent   float64 `json:"space_saved_percent"`
	DurationMs          int64   `json:"duration_ms"`
}

// Manifest is the lightweight "guide file" capable of reconstructing an entire project state.
type Manifest struct {
	ID          string        `json:"id"`
	ProjectID   string        `json:"project_id"`
	ProjectName string        `json:"project_name"`
	RootPath    string        `json:"root_path"`
	Tag         string        `json:"tag"`
	Message     string        `json:"message"`
	IsAutoStash bool          `json:"is_auto_stash"`
	Encrypted   bool          `json:"encrypted"`
	CreatedAt   time.Time     `json:"created_at"`
	Files       []FileEntry   `json:"files"`
	Stats       SnapshotStats `json:"stats"`
}

// ProjectRecord tracks a workspace or application managed by Permafrost.
type ProjectRecord struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Path             string    `json:"path"`
	ActiveSnapshotID string    `json:"active_snapshot_id"`
	ActiveTag        string    `json:"active_tag"`
	SnapshotCount    int       `json:"snapshot_count"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Registry stores all tracked projects and global metadata in ~/.permafrost/registry.json.
type Registry struct {
	Projects map[string]*ProjectRecord `json:"projects"`
}

// VaultStats summarizes global disk usage and deduplication savings across the entire vault.
type VaultStats struct {
	ProjectCount        int     `json:"project_count"`
	SnapshotCount       int     `json:"snapshot_count"`
	AutoStashCount      int     `json:"auto_stash_count"`
	UniqueBlobCount     int     `json:"unique_blob_count"`
	PhysicalVaultBytes  int64   `json:"physical_vault_bytes"`
	LogicalTrackedBytes int64   `json:"logical_tracked_bytes"`
	SavingsPercent      float64 `json:"savings_percent"`
}

// GCResult holds the summary of a Garbage Collection run.
type GCResult struct {
	ScannedBlobs      int   `json:"scanned_blobs"`
	ReferencedBlobs   int   `json:"referenced_blobs"`
	OrphanBlobsFreed  int   `json:"orphan_blobs_freed"`
	BytesReclaimed    int64 `json:"bytes_reclaimed"`
}

// Vault manages ~/.permafrost and all blobs, manifests, stashes, and binaries.
type Vault struct {
	RootDir  string
	Config   *config.GlobalConfig
	Registry *Registry
}

// Open initializes or opens the central Permafrost vault (~/.permafrost).
func Open(customVaultDir string) (*Vault, error) {
	cfg, err := config.LoadOrInit(customVaultDir)
	if err != nil {
		return nil, err
	}

	root := cfg.VaultDir
	subdirs := []string{
		filepath.Join(root, "store", "blobs"),
		filepath.Join(root, "store", "manifests"),
		filepath.Join(root, "stash"),
		filepath.Join(root, "bin"),
		filepath.Join(root, "teleports"),
	}
	for _, d := range subdirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return nil, fmt.Errorf("creating vault directory %s: %w", d, err)
		}
	}

	v := &Vault{
		RootDir: root,
		Config:  cfg,
		Registry: &Registry{
			Projects: make(map[string]*ProjectRecord),
		},
	}

	if err := v.loadRegistry(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *Vault) registryPath() string {
	return filepath.Join(v.RootDir, "registry.json")
}

func (v *Vault) loadRegistry() error {
	data, err := os.ReadFile(v.registryPath())
	if os.IsNotExist(err) {
		return v.SaveRegistry()
	} else if err != nil {
		return fmt.Errorf("reading registry: %w", err)
	}
	if err := json.Unmarshal(data, v.Registry); err != nil {
		return fmt.Errorf("parsing registry: %w", err)
	}
	if v.Registry.Projects == nil {
		v.Registry.Projects = make(map[string]*ProjectRecord)
	}
	return nil
}

// SaveRegistry persists ~/.permafrost/registry.json atomically.
func (v *Vault) SaveRegistry() error {
	tmp := v.registryPath() + ".tmp"
	data, err := json.MarshalIndent(v.Registry, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, v.registryPath())
}

// NormalizeProjectPath resolves an absolute clean path for consistent cross-platform indexing.
func NormalizeProjectPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	return abs, nil
}

// ComputeProjectID generates a deterministic short ID for a workspace path.
func ComputeProjectID(absPath string) string {
	key := absPath
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	sum := sha256.Sum256([]byte(key))
	base := filepath.Base(absPath)
	cleanBase := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, base)
	return fmt.Sprintf("%s-%s", strings.ToLower(cleanBase), hex.EncodeToString(sum[:4]))
}

// RegisterOrGetProject registers a project folder in the central vault or returns the existing record.
func (v *Vault) RegisterOrGetProject(projectPath, customName string) (*ProjectRecord, error) {
	absPath, err := NormalizeProjectPath(projectPath)
	if err != nil {
		return nil, err
	}
	id := ComputeProjectID(absPath)
	if rec, exists := v.Registry.Projects[id]; exists {
		if customName != "" && rec.Name != customName {
			rec.Name = customName
			rec.UpdatedAt = time.Now()
			_ = v.SaveRegistry()
		}
		return rec, nil
	}

	name := customName
	if name == "" {
		name = filepath.Base(absPath)
	}
	now := time.Now()
	rec := &ProjectRecord{
		ID:        id,
		Name:      name,
		Path:      absPath,
		CreatedAt: now,
		UpdatedAt: now,
	}
	v.Registry.Projects[id] = rec
	if err := os.MkdirAll(filepath.Join(v.RootDir, "store", "manifests", id), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(v.RootDir, "stash", id), 0755); err != nil {
		return nil, err
	}
	if err := v.SaveRegistry(); err != nil {
		return nil, err
	}
	v.AppendAuditLog("PROJECT_REGISTERED", fmt.Sprintf("Project %s (%s) at %s", rec.Name, rec.ID, rec.Path))
	return rec, nil
}

// FindProject searches by Path, ID, or Name.
func (v *Vault) FindProject(queryOrPath string) (*ProjectRecord, error) {
	if rec, ok := v.Registry.Projects[queryOrPath]; ok {
		return rec, nil
	}
	for _, rec := range v.Registry.Projects {
		if strings.EqualFold(rec.Name, queryOrPath) {
			return rec, nil
		}
	}
	if abs, err := NormalizeProjectPath(queryOrPath); err == nil {
		id := ComputeProjectID(abs)
		if rec, ok := v.Registry.Projects[id]; ok {
			return rec, nil
		}
	}
	return nil, fmt.Errorf("proyecto %q no está registrado en Permafrost (ejecuta 'pfrost init' o 'pfrost freeze')", queryOrPath)
}

// ListProjects returns all registered projects sorted by UpdatedAt descending.
func (v *Vault) ListProjects() []*ProjectRecord {
	list := make([]*ProjectRecord, 0, len(v.Registry.Projects))
	for _, p := range v.Registry.Projects {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].UpdatedAt.After(list[j].UpdatedAt)
	})
	return list
}

// BlobPath returns the content-addressed file path for a compressed chunk hash.
func (v *Vault) BlobPath(hash string) string {
	prefix := "00"
	if len(hash) >= 2 {
		prefix = hash[:2]
	}
	return filepath.Join(v.RootDir, "store", "blobs", prefix, hash+".zst")
}

// HasBlob checks if a compressed chunk already exists in the central vault.
func (v *Vault) HasBlob(hash string) (bool, int64) {
	st, err := os.Stat(v.BlobPath(hash))
	if err != nil {
		return false, 0
	}
	return true, st.Size()
}

// PutBlob writes a compressed chunk atomically to the content-addressed blob store.
func (v *Vault) PutBlob(hash string, compressedData []byte) error {
	p := v.BlobPath(hash)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	if _, err := os.Stat(p); err == nil {
		return nil // Already stored
	}
	tmp := fmt.Sprintf("%s.%d.tmp", p, time.Now().UnixNano())
	if err := os.WriteFile(tmp, compressedData, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// GetBlob reads a compressed chunk from the blob store.
func (v *Vault) GetBlob(hash string) ([]byte, error) {
	return os.ReadFile(v.BlobPath(hash))
}

// ManifestPath returns the path to a snapshot manifest (either normal or Safe Switch stash).
func (v *Vault) ManifestPath(projectID, snapshotID string, isStash bool) string {
	folder := filepath.Join(v.RootDir, "store", "manifests", projectID)
	if isStash {
		folder = filepath.Join(v.RootDir, "stash", projectID)
	}
	return filepath.Join(folder, snapshotID+".json")
}

// SaveManifest stores the lightweight snapshot guide file and updates the project registry.
func (v *Vault) SaveManifest(m *Manifest) error {
	p := v.ManifestPath(m.ProjectID, m.ID, m.IsAutoStash)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}

	if rec, ok := v.Registry.Projects[m.ProjectID]; ok && !m.IsAutoStash {
		rec.ActiveSnapshotID = m.ID
		rec.ActiveTag = m.Tag
		rec.UpdatedAt = time.Now()
		snaps, _ := v.ListManifests(m.ProjectID, false)
		rec.SnapshotCount = len(snaps)
		_ = v.SaveRegistry()
	}
	return nil
}

// LoadManifest resolves a snapshot by ID or Tag for a given project.
func (v *Vault) LoadManifest(projectID, idOrTag string, includeStashes bool) (*Manifest, error) {
	// Direct lookup first
	for _, isStash := range []bool{false, true} {
		if isStash && !includeStashes {
			continue
		}
		p := v.ManifestPath(projectID, idOrTag, isStash)
		if data, err := os.ReadFile(p); err == nil {
			var m Manifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, err
			}
			return &m, nil
		}
	}

	// Search by tag or ID prefix
	snaps, err := v.ListManifests(projectID, false)
	if err != nil {
		return nil, err
	}
	if includeStashes {
		stashes, _ := v.ListManifests(projectID, true)
		snaps = append(snaps, stashes...)
	}
	for _, m := range snaps {
		if strings.EqualFold(m.Tag, idOrTag) || strings.EqualFold(m.ID, idOrTag) || strings.HasPrefix(strings.ToLower(m.ID), strings.ToLower(idOrTag)) {
			return m, nil
		}
	}
	return nil, fmt.Errorf("versión o snapshot %q no encontrado para el proyecto %s", idOrTag, projectID)
}

// ListManifests returns all manifests for a project sorted newest first.
func (v *Vault) ListManifests(projectID string, isStash bool) ([]*Manifest, error) {
	folder := filepath.Join(v.RootDir, "store", "manifests", projectID)
	if isStash {
		folder = filepath.Join(v.RootDir, "stash", projectID)
	}
	entries, err := os.ReadDir(folder)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	var list []*Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(folder, e.Name()))
		if err != nil {
			continue
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err == nil {
			list = append(list, &m)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
	return list, nil
}

// DeleteManifest removes a snapshot or stash manifest and updates the project snapshot count.
func (v *Vault) DeleteManifest(projectID, idOrTag string, isStash bool) (*Manifest, error) {
	m, err := v.LoadManifest(projectID, idOrTag, isStash)
	if err != nil {
		return nil, err
	}
	p := v.ManifestPath(projectID, m.ID, m.IsAutoStash)
	if err := os.Remove(p); err != nil {
		return nil, err
	}
	if rec, ok := v.Registry.Projects[projectID]; ok && !m.IsAutoStash {
		snaps, _ := v.ListManifests(projectID, false)
		rec.SnapshotCount = len(snaps)
		if rec.ActiveSnapshotID == m.ID {
			if len(snaps) > 0 {
				rec.ActiveSnapshotID = snaps[0].ID
				rec.ActiveTag = snaps[0].Tag
			} else {
				rec.ActiveSnapshotID = ""
				rec.ActiveTag = ""
			}
		}
		_ = v.SaveRegistry()
	}
	v.AppendAuditLog("MANIFEST_DELETED", fmt.Sprintf("Deleted %s (%s) from %s", m.ID, m.Tag, projectID))
	return m, nil
}

// GarbageCollect removes any unreferenced chunks from ~/.permafrost/store/blobs.
func (v *Vault) GarbageCollect() (*GCResult, error) {
	referenced := make(map[string]struct{})

	// Scan all project manifests and stashes
	for projectID := range v.Registry.Projects {
		for _, isStash := range []bool{false, true} {
			manifests, _ := v.ListManifests(projectID, isStash)
			for _, m := range manifests {
				for _, f := range m.Files {
					for _, c := range f.Chunks {
						referenced[c.Hash] = struct{}{}
					}
				}
			}
		}
	}

	// Also scan Global Binaries manifests if present
	binManifests, _ := v.ListManifests("_global_binaries", false)
	for _, m := range binManifests {
		for _, f := range m.Files {
			for _, c := range f.Chunks {
				referenced[c.Hash] = struct{}{}
			}
		}
	}

	res := &GCResult{
		ReferencedBlobs: len(referenced),
	}

	blobsRoot := filepath.Join(v.RootDir, "store", "blobs")
	_ = filepath.Walk(blobsRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".zst") {
			return nil
		}
		res.ScannedBlobs++
		hash := strings.TrimSuffix(info.Name(), ".zst")
		if _, used := referenced[hash]; !used {
			size := info.Size()
			if rmErr := os.Remove(path); rmErr == nil {
				res.OrphanBlobsFreed++
				res.BytesReclaimed += size
			}
		}
		return nil
	})

	v.AppendAuditLog("GARBAGE_COLLECTION", fmt.Sprintf("Freed %d orphan blobs (%d bytes)", res.OrphanBlobsFreed, res.BytesReclaimed))
	return res, nil
}

// ComputeVaultStats calculates total physical vs logical space and deduplication ratio.
func (v *Vault) ComputeVaultStats() (*VaultStats, error) {
	stats := &VaultStats{
		ProjectCount: len(v.Registry.Projects),
	}
	for projectID := range v.Registry.Projects {
		snaps, _ := v.ListManifests(projectID, false)
		stats.SnapshotCount += len(snaps)
		for _, m := range snaps {
			stats.LogicalTrackedBytes += m.Stats.LogicalBytes
		}
		stashes, _ := v.ListManifests(projectID, true)
		stats.AutoStashCount += len(stashes)
		for _, m := range stashes {
			stats.LogicalTrackedBytes += m.Stats.LogicalBytes
		}
	}

	blobsRoot := filepath.Join(v.RootDir, "store", "blobs")
	_ = filepath.Walk(blobsRoot, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".zst") {
			stats.UniqueBlobCount++
			stats.PhysicalVaultBytes += info.Size()
		}
		return nil
	})

	if stats.LogicalTrackedBytes > 0 && stats.LogicalTrackedBytes > stats.PhysicalVaultBytes {
		saved := float64(stats.LogicalTrackedBytes - stats.PhysicalVaultBytes)
		stats.SavingsPercent = (saved / float64(stats.LogicalTrackedBytes)) * 100.0
	}
	return stats, nil
}

// AppendAuditLog writes a transparent human-readable line to ~/.permafrost/audit.log.
func (v *Vault) AppendAuditLog(action, details string) {
	logPath := filepath.Join(v.RootDir, "audit.log")
	line := fmt.Sprintf("[%s] %-22s | %s\n", time.Now().Format(time.RFC3339), action, details)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// ReadAuditLogs returns the last N lines from ~/.permafrost/audit.log for full transparency.
func (v *Vault) ReadAuditLogs(limit int) ([]string, error) {
	logPath := filepath.Join(v.RootDir, "audit.log")
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	rawLines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var out []string
	for _, l := range rawLines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}
