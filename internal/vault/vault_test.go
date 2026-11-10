package vault

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVaultProjectAndAuditLifecycle(t *testing.T) {
	vaultDir := t.TempDir()
	projDir := filepath.Join(t.TempDir(), "my-terraform-stack")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}

	v, err := Open(vaultDir)
	if err != nil {
		t.Fatalf("Open vault failed: %v", err)
	}

	rec, err := v.RegisterOrGetProject(projDir, "My Terraform Stack")
	if err != nil {
		t.Fatalf("RegisterOrGetProject failed: %v", err)
	}
	if rec.Name != "My Terraform Stack" {
		t.Fatalf("unexpected project name: %s", rec.Name)
	}

	found, err := v.FindProject(projDir)
	if err != nil || found.ID != rec.ID {
		t.Fatalf("FindProject by path failed: %v", err)
	}

	logs, err := v.ReadAuditLogs(10)
	if err != nil || len(logs) == 0 {
		t.Fatalf("expected audit logs to contain registration entry, got %d (%v)", len(logs), err)
	}
}
