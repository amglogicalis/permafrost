package engine

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/amglogicalis/permafrost/internal/vault"
)

// Engine coordinates Content-Defined Chunking (FastCDC), SHA-256 hashing, compression, and restoration.
type Engine struct {
	Vault      *vault.Vault
	chunker    *FastCDCChunker
	compLevel  int
}

// FreezeOptions configures how a snapshot is created.
type FreezeOptions struct {
	Tag          string
	Message      string
	ProjectName  string
	IsAutoStash  bool
	ExtraIgnores []string
}

// SwitchResult reports exact metrics from a version switch or selective restoration.
type SwitchResult struct {
	TargetSnapshotID      string          `json:"target_snapshot_id"`
	TargetTag             string          `json:"target_tag"`
	FilesCreated          int             `json:"files_created"`
	FilesUpdated          int             `json:"files_updated"`
	FilesSkippedUnchanged int             `json:"files_skipped_unchanged"`
	FilesRemoved          int             `json:"files_removed"`
	BytesWritten          int64           `json:"bytes_written"`
	DurationMs            int64           `json:"duration_ms"`
	AutoStashManifest     *vault.Manifest `json:"auto_stash_manifest,omitempty"`
}

// New creates a new deduplication and restoration Engine backed by the given Vault.
func New(v *vault.Vault) (*Engine, error) {
	cfg := v.Config.Chunking
	chunker := NewFastCDCChunker(cfg.MinChunkSize, cfg.AvgChunkSize, cfg.MaxChunkSize)

	level := zlib.DefaultCompression
	switch strings.ToLower(cfg.CompressionLevel) {
	case "fast":
		level = zlib.BestSpeed
	case "best":
		level = zlib.BestCompression
	}

	return &Engine{
		Vault:     v,
		chunker:   chunker,
		compLevel: level,
	}, nil
}

func (e *Engine) compressBlock(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(raw) / 2)
	w, err := zlib.NewWriterLevel(&buf, e.compLevel)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (e *Engine) decompressBlock(comp []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(comp))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// HashBytes returns the 64-char hex SHA-256 digest of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HashFile computes the full SHA-256 hash of a file on disk.
func HashFile(filePath string) (string, int64, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum), n, nil
}

// Freeze captures the state of projectPath using FastCDC + BLAKE3 + Zstd and writes a lightweight Manifest.
func (e *Engine) Freeze(projectPath string, opts FreezeOptions) (*vault.Manifest, error) {
	startTime := time.Now()
	absRoot, err := vault.NormalizeProjectPath(projectPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("no se puede acceder a la ruta %s: %w", absRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("la ruta %s no es un directorio (para binarios sueltos usa 'pfrost bin')", absRoot)
	}

	proj, err := e.Vault.RegisterOrGetProject(absRoot, opts.ProjectName)
	if err != nil {
		return nil, err
	}

	ignores := e.loadIgnorePatterns(absRoot, opts.ExtraIgnores)

	var entries []vault.FileEntry
	var stats vault.SnapshotStats
	seenInSnapshot := make(map[string]int64)

	err = filepath.Walk(absRoot, func(currentPath string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if currentPath == absRoot {
			return nil
		}

		rel, err := filepath.Rel(absRoot, currentPath)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)

		if shouldIgnore(relSlash, fi.IsDir(), ignores) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if fi.IsDir() {
			return nil
		}

		// Regular files only (skip irregular sockets/pipes)
		if !fi.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(currentPath)
		if err != nil {
			return fmt.Errorf("abriendo archivo %s: %w", relSlash, err)
		}
		defer f.Close()

		fileHasher := sha256.New()
		var chunks []vault.ChunkRef
		var fileSize int64

		splitErr := e.chunker.Split(f, func(rc RawChunk) error {
			_, _ = fileHasher.Write(rc.Data)
			cHash := HashBytes(rc.Data)
			rawLen := int64(len(rc.Data))
			fileSize += rawLen
			stats.TotalChunks++

			exists, compSize := e.Vault.HasBlob(cHash)
			if !exists {
				compressed, err := e.compressBlock(rc.Data)
				if err != nil {
					return err
				}
				if err := e.Vault.PutBlob(cHash, compressed); err != nil {
					return err
				}
				compSize = int64(len(compressed))
				stats.NewUniqueChunks++
				stats.NewBytesStored += compSize
			} else {
				stats.ReusedChunks++
			}

			if _, seen := seenInSnapshot[cHash]; !seen {
				seenInSnapshot[cHash] = compSize
				stats.TotalCompressedSize += compSize
			}

			chunks = append(chunks, vault.ChunkRef{
				Hash:           cHash,
				Offset:         rc.Offset,
				RawSize:        rawLen,
				CompressedSize: compSize,
			})
			return nil
		})
		if splitErr != nil {
			return fmt.Errorf("procesando bloques de %s: %w", relSlash, splitErr)
		}

		fullHash := hex.EncodeToString(fileHasher.Sum(nil))
		entries = append(entries, vault.FileEntry{
			RelPath:  relSlash,
			Size:     fileSize,
			Mode:     uint32(fi.Mode().Perm()),
			ModTime:  fi.ModTime().UTC().Truncate(time.Millisecond),
			FileHash: fullHash,
			Chunks:   chunks,
		})

		stats.TotalFiles++
		stats.LogicalBytes += fileSize
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].RelPath < entries[j].RelPath
	})

	if stats.LogicalBytes > 0 && stats.LogicalBytes > stats.NewBytesStored {
		stats.SpaceSavedPercent = (float64(stats.LogicalBytes-stats.NewBytesStored) / float64(stats.LogicalBytes)) * 100.0
	}
	stats.DurationMs = time.Since(startTime).Milliseconds()

	snapID := generateSnapshotID(opts.IsAutoStash)
	tag := strings.TrimSpace(opts.Tag)
	if tag == "" {
		if opts.IsAutoStash {
			tag = "auto-" + time.Now().Format("20060102-150405")
		} else {
			existing, _ := e.Vault.ListManifests(proj.ID, false)
			tag = fmt.Sprintf("v%d", len(existing)+1)
		}
	}

	manifest := &vault.Manifest{
		ID:          snapID,
		ProjectID:   proj.ID,
		ProjectName: proj.Name,
		RootPath:    absRoot,
		Tag:         tag,
		Message:     opts.Message,
		IsAutoStash: opts.IsAutoStash,
		CreatedAt:   time.Now().UTC(),
		Files:       entries,
		Stats:       stats,
	}

	if err := e.Vault.SaveManifest(manifest); err != nil {
		return nil, err
	}

	action := "SNAPSHOT_CREATED"
	if opts.IsAutoStash {
		action = "SAFE_SWITCH_AUTO_STASH"
	}
	e.Vault.AppendAuditLog(action, fmt.Sprintf("Project=%s ID=%s Tag=%s Files=%d Logical=%dB NewStored=%dB Saved=%.1f%%",
		proj.Name, manifest.ID, manifest.Tag, stats.TotalFiles, stats.LogicalBytes, stats.NewBytesStored, stats.SpaceSavedPercent))

	return manifest, nil
}

// Restore reconstructs a target Manifest (either full workspace hot-swap or selective file cherry-picking).
func (e *Engine) Restore(projectPath string, target *vault.Manifest, selectivePaths []string, cleanUntracked bool) (*SwitchResult, error) {
	startTime := time.Now()
	absRoot, err := vault.NormalizeProjectPath(projectPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absRoot, 0755); err != nil {
		return nil, err
	}

	res := &SwitchResult{
		TargetSnapshotID: target.ID,
		TargetTag:        target.Tag,
	}

	selectiveSet := make(map[string]bool)
	for _, p := range selectivePaths {
		norm := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(p)), "./")
		if norm != "" {
			selectiveSet[norm] = true
		}
	}
	isSelective := len(selectiveSet) > 0

	targetFileMap := make(map[string]vault.FileEntry, len(target.Files))
	for _, entry := range target.Files {
		if isSelective && !matchesSelective(entry.RelPath, selectiveSet) {
			continue
		}
		targetFileMap[entry.RelPath] = entry

		destPath := filepath.Join(absRoot, filepath.FromSlash(entry.RelPath))
		existingInfo, statErr := os.Stat(destPath)
		fileExisted := statErr == nil

		// Fast path: if file exists with same size, check full BLAKE3 hash. If identical, skip rewriting!
		if fileExisted && existingInfo.Mode().IsRegular() && existingInfo.Size() == entry.Size {
			if existingHash, _, hErr := HashFile(destPath); hErr == nil && existingHash == entry.FileHash {
				res.FilesSkippedUnchanged++
				continue
			}
		}

		// Reconstruct file from deduplicated chunks
		if err := e.reconstructFile(destPath, entry); err != nil {
			return nil, fmt.Errorf("reconstruyendo %s: %w", entry.RelPath, err)
		}
		res.BytesWritten += entry.Size
		if fileExisted {
			res.FilesUpdated++
		} else {
			res.FilesCreated++
		}
	}

	// If full switch and cleanUntracked is enabled, remove files not present in target snapshot
	if !isSelective && cleanUntracked {
		ignores := e.loadIgnorePatterns(absRoot, nil)
		_ = filepath.Walk(absRoot, func(currentPath string, fi os.FileInfo, walkErr error) error {
			if walkErr != nil || currentPath == absRoot {
				return nil
			}
			rel, err := filepath.Rel(absRoot, currentPath)
			if err != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			if shouldIgnore(relSlash, fi.IsDir(), ignores) {
				if fi.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !fi.IsDir() {
				if _, keep := targetFileMap[relSlash]; !keep {
					if rmErr := os.Remove(currentPath); rmErr == nil {
						res.FilesRemoved++
					}
				}
			}
			return nil
		})
		cleanEmptyDirs(absRoot, ignores)
	}

	if !isSelective {
		if rec, ok := e.Vault.Registry.Projects[target.ProjectID]; ok {
			rec.ActiveSnapshotID = target.ID
			rec.ActiveTag = target.Tag
			rec.UpdatedAt = time.Now()
			_ = e.Vault.SaveRegistry()
		}
	}

	res.DurationMs = time.Since(startTime).Milliseconds()
	e.Vault.AppendAuditLog("VERSION_RESTORED", fmt.Sprintf("Project=%s Target=%s (%s) Created=%d Updated=%d Skipped=%d Removed=%d Duration=%dms",
		target.ProjectName, target.ID, target.Tag, res.FilesCreated, res.FilesUpdated, res.FilesSkippedUnchanged, res.FilesRemoved, res.DurationMs))

	return res, nil
}

func (e *Engine) reconstructFile(destPath string, entry vault.FileEntry) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}

	tmpPath := fmt.Sprintf("%s.pfrost.%d.tmp", destPath, time.Now().UnixNano())
	outFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(entry.Mode)|0600)
	if err != nil {
		return err
	}

	fileHasher := sha256.New()
	for _, chunk := range entry.Chunks {
		compData, err := e.Vault.GetBlob(chunk.Hash)
		if err != nil {
			_ = outFile.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("bloque faltante %s en bóveda: %w", chunk.Hash, err)
		}
		rawBytes, err := e.decompressBlock(compData)
		if err != nil {
			_ = outFile.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("descomprimiendo bloque %s: %w", chunk.Hash, err)
		}
		if HashBytes(rawBytes) != chunk.Hash {
			_ = outFile.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("fallo de verificación de integridad BLAKE3 en bloque %s", chunk.Hash)
		}
		if _, err := outFile.Write(rawBytes); err != nil {
			_ = outFile.Close()
			_ = os.Remove(tmpPath)
			return err
		}
		_, _ = fileHasher.Write(rawBytes)
	}

	if err := outFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	gotFileHash := hex.EncodeToString(fileHasher.Sum(nil))
	if entry.FileHash != "" && gotFileHash != entry.FileHash {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("hash de archivo reconstruido no coincide (esperado %s, obtenido %s)", entry.FileHash, gotFileHash)
	}

	// On Windows, os.Rename over an existing read-only file can fail if permissions forbid write
	_ = os.Chmod(destPath, 0666)
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if entry.Mode != 0 {
		_ = os.Chmod(destPath, os.FileMode(entry.Mode))
	}
	if !entry.ModTime.IsZero() {
		_ = os.Chtimes(destPath, entry.ModTime, entry.ModTime)
	}
	return nil
}

// ScanWorkingTree performs a fast BLAKE3 scan of the current directory without writing blobs to the vault.
func (e *Engine) ScanWorkingTree(projectPath string) ([]vault.FileEntry, error) {
	absRoot, err := vault.NormalizeProjectPath(projectPath)
	if err != nil {
		return nil, err
	}
	ignores := e.loadIgnorePatterns(absRoot, nil)

	var entries []vault.FileEntry
	err = filepath.Walk(absRoot, func(currentPath string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil || currentPath == absRoot {
			return walkErr
		}
		rel, err := filepath.Rel(absRoot, currentPath)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if shouldIgnore(relSlash, fi.IsDir(), ignores) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if fi.IsDir() || !fi.Mode().IsRegular() {
			return nil
		}
		fHash, fSize, err := HashFile(currentPath)
		if err != nil {
			return err
		}
		entries = append(entries, vault.FileEntry{
			RelPath:  relSlash,
			Size:     fSize,
			Mode:     uint32(fi.Mode().Perm()),
			ModTime:  fi.ModTime().UTC().Truncate(time.Millisecond),
			FileHash: fHash,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].RelPath < entries[j].RelPath
	})
	return entries, nil
}

func (e *Engine) loadIgnorePatterns(absRoot string, extra []string) []string {
	patterns := make([]string, 0, len(e.Vault.Config.DefaultIgnores)+len(extra)+8)
	patterns = append(patterns, e.Vault.Config.DefaultIgnores...)
	patterns = append(patterns, extra...)

	ignoreFile := filepath.Join(absRoot, ".pfrostignore")
	if f, err := os.Open(ignoreFile); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			patterns = append(patterns, strings.TrimSuffix(filepath.ToSlash(line), "/"))
		}
	}
	return patterns
}

func shouldIgnore(relSlash string, isDir bool, patterns []string) bool {
	parts := strings.Split(relSlash, "/")
	base := parts[len(parts)-1]

	// Always ignore internal temporary reconstruction files
	if strings.Contains(base, ".pfrost.") && strings.HasSuffix(base, ".tmp") {
		return true
	}

	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if relSlash == pat || base == pat {
			return true
		}
		if strings.HasPrefix(relSlash, pat+"/") {
			return true
		}
		if matched, _ := path.Match(pat, base); matched {
			return true
		}
		if matched, _ := path.Match(pat, relSlash); matched {
			return true
		}
	}
	return false
}

func matchesSelective(relSlash string, selectiveSet map[string]bool) bool {
	if selectiveSet[relSlash] {
		return true
	}
	for sel := range selectiveSet {
		if strings.HasPrefix(relSlash, strings.TrimSuffix(sel, "/")+"/") {
			return true
		}
	}
	return false
}

func cleanEmptyDirs(absRoot string, ignores []string) {
	var dirs []string
	_ = filepath.Walk(absRoot, func(currentPath string, fi os.FileInfo, err error) error {
		if err == nil && fi.IsDir() && currentPath != absRoot {
			rel, relErr := filepath.Rel(absRoot, currentPath)
			if relErr == nil && !shouldIgnore(filepath.ToSlash(rel), true, ignores) {
				dirs = append(dirs, currentPath)
			}
		}
		return nil
	})
	// Remove deepest directories first if empty
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) > len(dirs[j])
	})
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err == nil && len(entries) == 0 {
			_ = os.Remove(d)
		}
	}
}

func generateSnapshotID(isStash bool) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	prefix := "snap"
	if isStash {
		prefix = "stash"
	}
	return fmt.Sprintf("%s_%s_%s", prefix, time.Now().UTC().Format("20060102_150405"), hex.EncodeToString(b[:]))
}
