package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amglogicalis/permafrost/internal/engine"
	"github.com/amglogicalis/permafrost/internal/vault"
	tea "github.com/charmbracelet/bubbletea"
)

func TestTUIWorkflowAndTabs(t *testing.T) {
	vaultDir := t.TempDir()
	projDir := t.TempDir()

	v, err := vault.Open(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(v)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(projDir, "app.go"), []byte("package main // v1"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Freeze(projDir, engine.FreezeOptions{Tag: "v1.0", Message: "Initial TUI test"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "app.go"), []byte("package main // modified in working tree"), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewAppModel(v, eng, projDir)

	// 1. Check TabSnapshots view
	view1 := m.View()
	if !strings.Contains(view1, "v1.0") || !strings.Contains(view1, "Initial TUI test") {
		t.Fatalf("expected TabSnapshots view to show v1.0 snapshot, got:\n%s", view1)
	}

	// 2. Navigate to TabDiff ('d')
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if m.ActiveTab != TabDiff {
		t.Fatalf("expected active tab to be TabDiff after pressing 'd'")
	}
	view2 := m.View()
	if !strings.Contains(view2, "app.go") || !strings.Contains(view2, "MODIFICADO") {
		t.Fatalf("expected TabDiff view to display modified app.go, got:\n%s", view2)
	}

	// 3. Navigate to TabFeatures ('3') and toggle first feature (Safe Switch) with Space
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	if m.ActiveTab != TabFeatures {
		t.Fatalf("expected active tab to be TabFeatures")
	}
	if !v.Config.Features.SafeSwitch {
		t.Fatalf("expected SafeSwitch to start enabled")
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if v.Config.Features.SafeSwitch {
		t.Fatalf("expected SafeSwitch to be toggled OFF after pressing Space in TabFeatures")
	}
	// Toggle back ON
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if !v.Config.Features.SafeSwitch {
		t.Fatalf("expected SafeSwitch to be toggled back ON")
	}

	// 4. Navigate to TabAudit ('4')
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'4'}})
	view4 := m.View()
	if !strings.Contains(view4, "FEATURE_TOGGLED") {
		t.Fatalf("expected TabAudit view to show FEATURE_TOGGLED audit log entry, got:\n%s", view4)
	}
}
