package safeswitch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/amglogicalis/permafrost/internal/engine"
	"github.com/amglogicalis/permafrost/internal/vault"
)

func TestSafeSwitchToggleableAndPromote(t *testing.T) {
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
	ss := New(v, eng)

	// Create v1 and v2
	appFile := filepath.Join(projDir, "index.html")
	if err := os.WriteFile(appFile, []byte("<h1>Version 1</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	m1, err := eng.Freeze(projDir, engine.FreezeOptions{Tag: "v1.0", Message: "Base v1"})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(appFile, []byte("<h1>Version 2</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = eng.Freeze(projDir, engine.FreezeOptions{Tag: "v2.0", Message: "Base v2"})
	if err != nil {
		t.Fatal(err)
	}

	// Make unsaved edits on top of v2
	unsavedContent := "<h1>Work in Progress Unsaved Changes</h1>"
	if err := os.WriteFile(appFile, []byte(unsavedContent), 0644); err != nil {
		t.Fatal(err)
	}

	proj, err := v.FindProject(projDir)
	if err != nil {
		t.Fatal(err)
	}

	// 1. With SafeSwitch = ON (default), switching to v1 must create an auto-stash
	stash, rep, err := ss.CheckAndStash(proj, m1)
	if err != nil {
		t.Fatalf("CheckAndStash failed: %v", err)
	}
	if stash == nil || !stash.IsAutoStash || rep.ModifiedCount != 1 {
		t.Fatalf("expected auto-stash to be created for 1 modified file, got stash=%v", stash)
	}

	// Complete switch to v1
	if _, err := eng.Restore(projDir, m1, nil, true); err != nil {
		t.Fatal(err)
	}

	// 2. Restore the auto-stash and verify our unsaved changes come back 100% intact
	_, _, err = ss.RestoreStash(proj, stash.ID, false)
	if err != nil {
		t.Fatalf("RestoreStash failed: %v", err)
	}
	gotBytes, err := os.ReadFile(appFile)
	if err != nil || string(gotBytes) != unsavedContent {
		t.Fatalf("expected restored stash content %q, got %q", unsavedContent, string(gotBytes))
	}

	// 3. Promote the auto-stash to a permanent snapshot ("v2.1-recovered")
	promoted, err := ss.PromoteStash(proj, stash.ID, "v2.1-recovered", "Promoted from Safe Switch")
	if err != nil {
		t.Fatalf("PromoteStash failed: %v", err)
	}
	if promoted.IsAutoStash || promoted.Tag != "v2.1-recovered" {
		t.Fatalf("expected promoted snapshot to be permanent with tag v2.1-recovered")
	}

	// 4. Toggle SafeSwitch = OFF and verify CheckAndStash returns nil
	if err := v.Config.SetFeature("safe_switch", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appFile, []byte("another edit while OFF"), 0644); err != nil {
		t.Fatal(err)
	}
	stashWhenOff, _, err := ss.CheckAndStash(proj, m1)
	if err != nil || stashWhenOff != nil {
		t.Fatalf("expected no auto-stash when SafeSwitch is OFF, got %v (%v)", stashWhenOff, err)
	}
}
