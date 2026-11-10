package cloud

import (
	"crypto/rand"
	"encoding/hex"
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

// TeleportCapsule represents a shareable environment capsule stored in ~/.permafrost/teleports/ or Cloud.
type TeleportCapsule struct {
	Code        string            `json:"code"`
	ProjectName string            `json:"project_name"`
	Tag         string            `json:"tag"`
	SnapshotID  string            `json:"snapshot_id"`
	Encrypted   bool              `json:"encrypted"`
	CreatedAt   time.Time         `json:"created_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	CapsulePath string            `json:"capsule_path"`
	Index       *BundleIndex      `json:"index"`
	Chunks      map[string][]byte `json:"chunks"`
}

// IsTeleportEnabled returns whether the Teleport module is currently ON.
func (m *Manager) IsTeleportEnabled() bool {
	return m.Vault.Config.Features.Teleport
}

// CreateTeleport packages a snapshot into an encrypted (or plain) Teleport Capsule with an optional TTL.
func (m *Manager) CreateTeleport(manifest *vault.Manifest, passphrase string, ttl time.Duration) (*TeleportCapsule, error) {
	if !m.IsTeleportEnabled() {
		return nil, fmt.Errorf("el módulo Teleport está DESACTIVADO [OFF]. Actívalo con 'pfrost feature teleport on'")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}

	idx, chunks, err := m.PackToChunks(manifest, passphrase, 0)
	if err != nil {
		return nil, err
	}

	var randBytes [4]byte
	_, _ = rand.Read(randBytes[:])
	code := "tp_" + hex.EncodeToString(randBytes[:])

	now := time.Now().UTC()
	capsulePath := filepath.Join(m.Vault.RootDir, "teleports", code+".pfteleport")
	if err := os.MkdirAll(filepath.Dir(capsulePath), 0755); err != nil {
		return nil, err
	}

	capsule := &TeleportCapsule{
		Code:        code,
		ProjectName: manifest.ProjectName,
		Tag:         manifest.Tag,
		SnapshotID:  manifest.ID,
		Encrypted:   idx.Encrypted,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
		CapsulePath: capsulePath,
		Index:       idx,
		Chunks:      chunks,
	}

	data, err := json.MarshalIndent(capsule, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(capsulePath, data, 0600); err != nil {
		return nil, err
	}

	m.Vault.AppendAuditLog("TELEPORT_CREATED", fmt.Sprintf("Code=%s Project=%s Tag=%s Encrypted=%v Expires=%s",
		code, manifest.ProjectName, manifest.Tag, idx.Encrypted, capsule.ExpiresAt.Format(time.RFC3339)))

	return capsule, nil
}

// ReceiveTeleport loads a Teleport Capsule by code or file path, checks expiration, verifies SHA-256 and decrypts,
// and restores the exact workspace into destDir.
func (m *Manager) ReceiveTeleport(eng *engine.Engine, codeOrFilePath, destDir, passphrase string) (*vault.Manifest, *engine.SwitchResult, error) {
	if !m.IsTeleportEnabled() {
		return nil, nil, fmt.Errorf("el módulo Teleport está DESACTIVADO [OFF]. Actívalo con 'pfrost feature teleport on'")
	}

	capsulePath := codeOrFilePath
	if !strings.HasSuffix(strings.ToLower(capsulePath), ".pfteleport") {
		code := strings.TrimSpace(codeOrFilePath)
		if !strings.HasPrefix(code, "tp_") {
			code = "tp_" + code
		}
		capsulePath = filepath.Join(m.Vault.RootDir, "teleports", code+".pfteleport")
	}

	data, err := os.ReadFile(capsulePath)
	if err != nil {
		return nil, nil, fmt.Errorf("no se encontró la cápsula Teleport %q: %w", codeOrFilePath, err)
	}

	var capsule TeleportCapsule
	if err := json.Unmarshal(data, &capsule); err != nil {
		return nil, nil, fmt.Errorf("cápsula Teleport inválida: %w", err)
	}

	if !capsule.ExpiresAt.IsZero() && time.Now().UTC().After(capsule.ExpiresAt) {
		return nil, nil, fmt.Errorf("el código Teleport %s expiró el %s", capsule.Code, capsule.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	}

	manifest, err := m.UnpackFromChunks(capsule.Index, capsule.Chunks, passphrase, destDir)
	if err != nil {
		return nil, nil, err
	}

	res, err := eng.Restore(destDir, manifest, nil, true)
	if err != nil {
		return manifest, nil, err
	}

	m.Vault.AppendAuditLog("TELEPORT_RECEIVED", fmt.Sprintf("Code=%s Dest=%s Files=%d",
		capsule.Code, destDir, manifest.Stats.TotalFiles))

	return manifest, res, nil
}

// ListTeleports returns all active Teleport capsules stored in ~/.permafrost/teleports.
func (m *Manager) ListTeleports() ([]*TeleportCapsule, error) {
	dir := filepath.Join(m.Vault.RootDir, "teleports")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	var list []*TeleportCapsule
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pfteleport") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cap TeleportCapsule
		if json.Unmarshal(data, &cap) == nil {
			cap.CapsulePath = p
			// Omit raw chunk bytes from summary listing
			cap.Chunks = nil
			list = append(list, &cap)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
	return list, nil
}

// RevokeTeleport deletes a Teleport capsule by code (or "all" to clear all teleports).
func (m *Manager) RevokeTeleport(codeOrAll string) (int, error) {
	codeOrAll = strings.TrimSpace(codeOrAll)
	if strings.EqualFold(codeOrAll, "all") {
		list, _ := m.ListTeleports()
		count := 0
		for _, c := range list {
			if os.Remove(c.CapsulePath) == nil {
				count++
			}
		}
		m.Vault.AppendAuditLog("TELEPORT_REVOKED_ALL", fmt.Sprintf("Count=%d", count))
		return count, nil
	}

	if !strings.HasPrefix(codeOrAll, "tp_") {
		codeOrAll = "tp_" + codeOrAll
	}
	p := filepath.Join(m.Vault.RootDir, "teleports", codeOrAll+".pfteleport")
	if err := os.Remove(p); err != nil {
		return 0, fmt.Errorf("no se encontró el Teleport %s", codeOrAll)
	}
	m.Vault.AppendAuditLog("TELEPORT_REVOKED", fmt.Sprintf("Code=%s", codeOrAll))
	return 1, nil
}
