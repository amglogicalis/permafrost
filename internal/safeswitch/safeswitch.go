package safeswitch

import (
	"fmt"
	"strings"
	"time"

	"github.com/amglogicalis/permafrost/internal/diff"
	"github.com/amglogicalis/permafrost/internal/engine"
	"github.com/amglogicalis/permafrost/internal/vault"
)

// Manager handles Safe Switch auto-stashes with full transparency and user control.
type Manager struct {
	Vault  *vault.Vault
	Engine *engine.Engine
}

// New creates a Safe Switch Manager.
func New(v *vault.Vault, eng *engine.Engine) *Manager {
	return &Manager{
		Vault:  v,
		Engine: eng,
	}
}

// IsEnabled returns whether Safe Switch is currently active.
func (m *Manager) IsEnabled() bool {
	return m.Vault.Config.Features.SafeSwitch
}

// CheckAndStash inspects the working directory before a version switch.
// If Safe Switch is ON and there are uncommitted changes compared to the active snapshot (or target snapshot),
// it automatically creates a lightweight auto-stash in ~/.permafrost/stash/<project-id>/.
func (m *Manager) CheckAndStash(proj *vault.ProjectRecord, target *vault.Manifest) (*vault.Manifest, *diff.Report, error) {
	if !m.IsEnabled() {
		return nil, nil, nil
	}

	// Compare working directory against the currently active snapshot (if one exists), otherwise against target
	baseline := target
	if proj.ActiveSnapshotID != "" {
		if activeSnap, err := m.Vault.LoadManifest(proj.ID, proj.ActiveSnapshotID, false); err == nil {
			baseline = activeSnap
		}
	}

	rep, err := diff.CompareWorkingTreeToSnapshot(m.Engine, proj.Path, baseline, false)
	if err != nil {
		return nil, nil, err
	}

	// If working tree has no modifications compared to baseline, no stash is needed
	if !rep.HasChanges() {
		return nil, rep, nil
	}

	tag := fmt.Sprintf("auto-pre-%s-%s", sanitizeTag(target.Tag), time.Now().Format("150405"))
	msg := fmt.Sprintf("Auto-Stash antes de cambiar a %s (+%d ~%d -%d archivos)",
		target.Tag, rep.AddedCount, rep.ModifiedCount, rep.RemovedCount)

	stashManifest, err := m.Engine.Freeze(proj.Path, engine.FreezeOptions{
		Tag:         tag,
		Message:     msg,
		ProjectName: proj.Name,
		IsAutoStash: true,
	})
	if err != nil {
		return nil, rep, fmt.Errorf("creando auto-stash de Safe Switch: %w", err)
	}

	// Enforce maximum auto-stashes rotation policy
	m.pruneOldStashes(proj.ID)

	return stashManifest, rep, nil
}

// ListStashes returns all auto-stashes for a project sorted newest first.
func (m *Manager) ListStashes(projectID string) ([]*vault.Manifest, error) {
	return m.Vault.ListManifests(projectID, true)
}

// RestoreStash applies an auto-stash back onto the project workspace.
func (m *Manager) RestoreStash(proj *vault.ProjectRecord, stashIDOrTag string, dropAfter bool) (*engine.SwitchResult, *vault.Manifest, error) {
	stash, err := m.Vault.LoadManifest(proj.ID, stashIDOrTag, true)
	if err != nil {
		return nil, nil, err
	}
	res, err := m.Engine.Restore(proj.Path, stash, nil, true)
	if err != nil {
		return nil, nil, err
	}
	if dropAfter {
		_, _ = m.Vault.DeleteManifest(proj.ID, stash.ID, true)
	}
	m.Vault.AppendAuditLog("SAFE_SWITCH_RESTORED", fmt.Sprintf("Project=%s Stash=%s (%s) DropAfter=%v",
		proj.Name, stash.ID, stash.Tag, dropAfter))
	return res, stash, nil
}

// PromoteStash converts a temporary Safe Switch auto-stash into a permanent project snapshot.
func (m *Manager) PromoteStash(proj *vault.ProjectRecord, stashIDOrTag, newTag, newMsg string) (*vault.Manifest, error) {
	stash, err := m.Vault.LoadManifest(proj.ID, stashIDOrTag, true)
	if err != nil {
		return nil, err
	}
	if !stash.IsAutoStash {
		return nil, fmt.Errorf("el snapshot %s ya es una versión permanente", stash.Tag)
	}

	oldID := stash.ID
	stash.IsAutoStash = false
	stash.ID = strings.Replace(stash.ID, "stash_", "snap_", 1)
	if strings.TrimSpace(newTag) != "" {
		stash.Tag = strings.TrimSpace(newTag)
	}
	if strings.TrimSpace(newMsg) != "" {
		stash.Message = strings.TrimSpace(newMsg)
	}

	if err := m.Vault.SaveManifest(stash); err != nil {
		return nil, err
	}
	_, _ = m.Vault.DeleteManifest(proj.ID, oldID, true)
	m.Vault.AppendAuditLog("SAFE_SWITCH_PROMOTED", fmt.Sprintf("Promoted %s -> %s (%s)", oldID, stash.ID, stash.Tag))
	return stash, nil
}

// ClearStashes removes all auto-stashes for a project and returns the count deleted.
func (m *Manager) ClearStashes(projectID string) (int, error) {
	stashes, err := m.ListStashes(projectID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, s := range stashes {
		if _, err := m.Vault.DeleteManifest(projectID, s.ID, true); err == nil {
			count++
		}
	}
	m.Vault.AppendAuditLog("SAFE_SWITCH_CLEARED", fmt.Sprintf("Project=%s Deleted=%d auto-stashes", projectID, count))
	return count, nil
}

func (m *Manager) pruneOldStashes(projectID string) {
	maxStashes := m.Vault.Config.Chunking.MaxAutoStashes
	if maxStashes <= 0 {
		maxStashes = 15
	}
	stashes, err := m.ListStashes(projectID)
	if err != nil || len(stashes) <= maxStashes {
		return
	}
	for _, old := range stashes[maxStashes:] {
		_, _ = m.Vault.DeleteManifest(projectID, old.ID, true)
	}
}

func sanitizeTag(t string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			return r
		}
		return '-'
	}, t)
}
