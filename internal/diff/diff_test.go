package diff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amglogicalis/permafrost/internal/engine"
	"github.com/amglogicalis/permafrost/internal/vault"
)

func TestTimeTravelDiffAndCherryPick(t *testing.T) {
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

	// Create v1 with .env, main.tf, and obsolete.txt
	if err := os.WriteFile(filepath.Join(projDir, ".env"), []byte("API_KEY=secret_v1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "main.tf"), []byte("version = 1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "obsolete.txt"), []byte("remove me in v2"), 0644); err != nil {
		t.Fatal(err)
	}

	m1, err := eng.Freeze(projDir, engine.FreezeOptions{Tag: "v1.0", Message: "Base v1"})
	if err != nil {
		t.Fatal(err)
	}

	// Modify .env, remove obsolete.txt, add new_feature.go
	if err := os.WriteFile(filepath.Join(projDir, ".env"), []byte("API_KEY=secret_v2_updated_longer"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(projDir, "obsolete.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "new_feature.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	m2, err := eng.Freeze(projDir, engine.FreezeOptions{Tag: "v2.0", Message: "Updated v2"})
	if err != nil {
		t.Fatal(err)
	}

	// Compare v1 -> v2
	rep := CompareEntries("v1.0", m1.Files, "v2.0", m2.Files)
	if rep.AddedCount != 1 || rep.ModifiedCount != 1 || rep.RemovedCount != 1 || rep.UnchangedCount != 1 {
		t.Fatalf("unexpected diff counts: +1=%d ~1=%d -1=%d =1=%d",
			rep.AddedCount, rep.ModifiedCount, rep.RemovedCount, rep.UnchangedCount)
	}

	colored := rep.FormatColored(false)
	if !strings.Contains(colored, "new_feature.go") || !strings.Contains(colored, ".env") || !strings.Contains(colored, "obsolete.txt") {
		t.Fatalf("colored output missing expected filenames:\n%s", colored)
	}

	// Test Selective Cherry-Pick: bring ONLY ".env" from v1.0 without touching new_feature.go!
	res, err := eng.Restore(projDir, m1, []string{".env"}, false)
	if err != nil {
		t.Fatalf("cherry-pick restore failed: %v", err)
	}
	if res.FilesUpdated != 1 || res.FilesRemoved != 0 {
		t.Fatalf("expected 1 updated file and 0 removed on cherry-pick, got updated=%d removed=%d",
			res.FilesUpdated, res.FilesRemoved)
	}

	envBytes, err := os.ReadFile(filepath.Join(projDir, ".env"))
	if err != nil || string(envBytes) != "API_KEY=secret_v1" {
		t.Fatalf("expected .env to be restored to v1 content, got %q", string(envBytes))
	}
	if _, err := os.Stat(filepath.Join(projDir, "new_feature.go")); err != nil {
		t.Fatalf("expected new_feature.go to remain untouched during selective cherry-pick: %v", err)
	}
}
