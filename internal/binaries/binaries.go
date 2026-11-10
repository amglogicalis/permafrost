package binaries

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/amglogicalis/permafrost/internal/engine"
	"github.com/amglogicalis/permafrost/internal/vault"
)

const globalBinProjectID = "_global_binaries"

// ToolVersion describes a single frozen version of a standalone binary tool.
type ToolVersion struct {
	VersionTag     string    `json:"version_tag"`
	ManifestID     string    `json:"manifest_id"`
	ExecutableName string    `json:"executable_name"`
	LogicalBytes   int64     `json:"logical_bytes"`
	NewBytesStored int64     `json:"new_bytes_stored"`
	FileHash       string    `json:"file_hash"`
	Note           string    `json:"note"`
	CreatedAt      time.Time `json:"created_at"`
}

// ToolRecord tracks a standalone binary tool (e.g. terraform, kubectl, hugo) and all its versions.
type ToolRecord struct {
	Name          string                 `json:"name"`
	ActiveVersion string                 `json:"active_version"`
	ActivePath    string                 `json:"active_path"`
	Versions      map[string]ToolVersion `json:"versions"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

// Catalog persists all managed global binaries in ~/.permafrost/binaries.json.
type Catalog struct {
	Tools map[string]*ToolRecord `json:"tools"`
}

// Manager handles registration, FastCDC deduplication, and instant switching of standalone CLI binaries.
type Manager struct {
	Vault   *vault.Vault
	Engine  *engine.Engine
	Catalog *Catalog
}

// New creates a Global Binaries Manager backed by the central Vault.
func New(v *vault.Vault, eng *engine.Engine) (*Manager, error) {
	m := &Manager{
		Vault:  v,
		Engine: eng,
		Catalog: &Catalog{
			Tools: make(map[string]*ToolRecord),
		},
	}
	if err := os.MkdirAll(m.BinDir(), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(v.RootDir, "store", "manifests", globalBinProjectID), 0755); err != nil {
		return nil, err
	}
	if err := m.loadCatalog(); err != nil {
		return nil, err
	}
	return m, nil
}

// IsEnabled returns whether the Global Binaries module is currently ON.
func (m *Manager) IsEnabled() bool {
	return m.Vault.Config.Features.GlobalBinaries
}

// BinDir returns ~/.permafrost/bin.
func (m *Manager) BinDir() string {
	return filepath.Join(m.Vault.RootDir, "bin")
}

// IsInSystemPath checks if ~/.permafrost/bin is present in the current PATH environment variable.
func (m *Manager) IsInSystemPath() bool {
	pathEnv := os.Getenv("PATH")
	cleanBin := strings.ToLower(filepath.Clean(m.BinDir()))
	for _, part := range filepath.SplitList(pathEnv) {
		if strings.ToLower(filepath.Clean(part)) == cleanBin {
			return true
		}
	}
	return false
}

// AddBinary freezes a standalone executable file into the deduplicated Vault and activates it in ~/.permafrost/bin.
func (m *Manager) AddBinary(toolName, binaryFilePath, versionTag, note string) (*ToolVersion, error) {
	if !m.IsEnabled() {
		return nil, fmt.Errorf("el módulo Global Binaries está DESACTIVADO [OFF]. Actívalo con 'pfrost feature global_binaries on'")
	}

	toolName = strings.ToLower(strings.TrimSpace(toolName))
	versionTag = strings.TrimSpace(versionTag)
	if toolName == "" || versionTag == "" {
		return nil, fmt.Errorf("el nombre de la herramienta y la versión son obligatorios")
	}

	info, err := os.Stat(binaryFilePath)
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("el archivo binario %q no existe o es un directorio", binaryFilePath)
	}

	// Stage the binary in a temporary directory to freeze via the FastCDC Engine
	stageDir, err := os.MkdirTemp("", "pfrost-bin-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stageDir)

	execName := toolName
	if strings.HasSuffix(strings.ToLower(binaryFilePath), ".exe") && !strings.HasSuffix(execName, ".exe") {
		execName += ".exe"
	}

	stagedFile := filepath.Join(stageDir, execName)
	rawData, err := os.ReadFile(binaryFilePath)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(stagedFile, rawData, 0755); err != nil {
		return nil, err
	}

	manifest, err := m.Engine.Freeze(stageDir, engine.FreezeOptions{
		Tag:         fmt.Sprintf("%s-%s", toolName, versionTag),
		Message:     note,
		ProjectName: fmt.Sprintf("bin:%s", toolName),
	})
	if err != nil {
		return nil, err
	}

	// Move manifest to _global_binaries namespace and remove temporary project registration
	oldProjID := manifest.ProjectID
	manifest.ProjectID = globalBinProjectID
	manifest.ID = fmt.Sprintf("bin_%s_%s_%d", sanitizeIdentifier(toolName), sanitizeIdentifier(versionTag), time.Now().Unix())
	if err := m.Vault.SaveManifest(manifest); err != nil {
		return nil, err
	}
	delete(m.Vault.Registry.Projects, oldProjID)
	_ = m.Vault.SaveRegistry()
	_ = os.RemoveAll(filepath.Join(m.Vault.RootDir, "store", "manifests", oldProjID))
	_ = os.RemoveAll(filepath.Join(m.Vault.RootDir, "stash", oldProjID))

	fileHash := ""
	if len(manifest.Files) > 0 {
		fileHash = manifest.Files[0].FileHash
	}

	tv := ToolVersion{
		VersionTag:     versionTag,
		ManifestID:     manifest.ID,
		ExecutableName: execName,
		LogicalBytes:   manifest.Stats.LogicalBytes,
		NewBytesStored: manifest.Stats.NewBytesStored,
		FileHash:       fileHash,
		Note:           note,
		CreatedAt:      time.Now().UTC(),
	}

	rec, exists := m.Catalog.Tools[toolName]
	if !exists {
		rec = &ToolRecord{
			Name:     toolName,
			Versions: make(map[string]ToolVersion),
		}
		m.Catalog.Tools[toolName] = rec
	}
	rec.Versions[versionTag] = tv
	rec.UpdatedAt = time.Now().UTC()
	if err := m.saveCatalog(); err != nil {
		return nil, err
	}

	m.Vault.AppendAuditLog("BINARY_ADDED", fmt.Sprintf("Tool=%s Version=%s Logical=%dB NewStored=%dB",
		toolName, versionTag, tv.LogicalBytes, tv.NewBytesStored))

	// Activate this version immediately in ~/.permafrost/bin
	if _, err := m.UseVersion(toolName, versionTag); err != nil {
		return &tv, err
	}
	return &tv, nil
}

// UseVersion hot-swaps the active executable for toolName in ~/.permafrost/bin/<tool>.
func (m *Manager) UseVersion(toolName, versionTag string) (string, error) {
	if !m.IsEnabled() {
		return "", fmt.Errorf("el módulo Global Binaries está DESACTIVADO [OFF]. Actívalo con 'pfrost feature global_binaries on'")
	}

	toolName = strings.ToLower(strings.TrimSpace(toolName))
	rec, exists := m.Catalog.Tools[toolName]
	if !exists {
		return "", fmt.Errorf("la herramienta %q no está registrada en Permafrost Global Binaries", toolName)
	}
	tv, vExists := rec.Versions[versionTag]
	if !vExists {
		return "", fmt.Errorf("la versión %q no existe para %s", versionTag, toolName)
	}

	manifest, err := m.Vault.LoadManifest(globalBinProjectID, tv.ManifestID, false)
	if err != nil {
		return "", err
	}

	// Reconstruct only this binary into ~/.permafrost/bin without deleting other tools in ~/.permafrost/bin
	if _, err := m.Engine.Restore(m.BinDir(), manifest, []string{tv.ExecutableName}, false); err != nil {
		return "", err
	}

	activePath := filepath.Join(m.BinDir(), tv.ExecutableName)
	_ = os.Chmod(activePath, 0755)

	rec.ActiveVersion = versionTag
	rec.ActivePath = activePath
	rec.UpdatedAt = time.Now().UTC()
	if err := m.saveCatalog(); err != nil {
		return "", err
	}

	m.Vault.AppendAuditLog("BINARY_SWITCHED", fmt.Sprintf("Tool=%s ActiveVersion=%s Path=%s", toolName, versionTag, activePath))
	return activePath, nil
}

// RemoveVersion deletes a frozen version of a binary tool.
func (m *Manager) RemoveVersion(toolName, versionTag string) error {
	toolName = strings.ToLower(strings.TrimSpace(toolName))
	rec, exists := m.Catalog.Tools[toolName]
	if !exists {
		return fmt.Errorf("la herramienta %q no existe", toolName)
	}
	tv, vExists := rec.Versions[versionTag]
	if !vExists {
		return fmt.Errorf("la versión %q no existe para %s", versionTag, toolName)
	}

	_, _ = m.Vault.DeleteManifest(globalBinProjectID, tv.ManifestID, false)
	delete(rec.Versions, versionTag)
	if rec.ActiveVersion == versionTag {
		rec.ActiveVersion = ""
		_ = os.Remove(rec.ActivePath)
		rec.ActivePath = ""
	}
	if len(rec.Versions) == 0 {
		delete(m.Catalog.Tools, toolName)
	}
	m.Vault.AppendAuditLog("BINARY_REMOVED", fmt.Sprintf("Tool=%s Version=%s", toolName, versionTag))
	return m.saveCatalog()
}

// ListTools returns all managed binary tools sorted alphabetically.
func (m *Manager) ListTools() []*ToolRecord {
	var list []*ToolRecord
	for _, r := range m.Catalog.Tools {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Name < list[j].Name
	})
	return list
}

func (m *Manager) catalogPath() string {
	return filepath.Join(m.Vault.RootDir, "binaries.json")
}

func (m *Manager) loadCatalog() error {
	data, err := os.ReadFile(m.catalogPath())
	if os.IsNotExist(err) {
		return m.saveCatalog()
	} else if err != nil {
		return err
	}
	if err := json.Unmarshal(data, m.Catalog); err != nil {
		return err
	}
	if m.Catalog.Tools == nil {
		m.Catalog.Tools = make(map[string]*ToolRecord)
	}
	return nil
}

func (m *Manager) saveCatalog() error {
	data, err := json.MarshalIndent(m.Catalog, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.catalogPath(), data, 0600)
}

func sanitizeIdentifier(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
}
