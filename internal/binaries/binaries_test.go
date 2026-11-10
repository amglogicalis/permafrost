package binaries

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/amglogicalis/permafrost/internal/engine"
	"github.com/amglogicalis/permafrost/internal/vault"
)

func TestGlobalBinariesManagerHotSwapAndToggle(t *testing.T) {
	vaultDir := t.TempDir()
	tmpSrc := t.TempDir()

	v, err := vault.Open(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(v)
	if err != nil {
		t.Fatal(err)
	}
	bm, err := New(v, eng)
	if err != nil {
		t.Fatal(err)
	}

	// Create two mock binary executables (v1.7.0 and v1.8.0)
	binV17 := filepath.Join(tmpSrc, "terraform-1.7")
	binV18 := filepath.Join(tmpSrc, "terraform-1.8")
	if err := os.WriteFile(binV17, []byte("#!/bin/sh\necho 'Terraform v1.7.0'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binV18, []byte("#!/bin/sh\necho 'Terraform v1.8.0'\n"), 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Add terraform v1.7.0 and v1.8.0
	if _, err := bm.AddBinary("terraform", binV17, "v1.7.0", "Stable 1.7"); err != nil {
		t.Fatalf("AddBinary v1.7.0 failed: %v", err)
	}
	if _, err := bm.AddBinary("terraform", binV18, "v1.8.0", "Latest 1.8"); err != nil {
		t.Fatalf("AddBinary v1.8.0 failed: %v", err)
	}

	// Verify active binary in ~/.permafrost/bin/terraform is currently v1.8.0
	activeBinPath := filepath.Join(bm.BinDir(), "terraform")
	content, err := os.ReadFile(activeBinPath)
	if err != nil || string(content) != "#!/bin/sh\necho 'Terraform v1.8.0'\n" {
		t.Fatalf("expected active binary to be v1.8.0, got %q (%v)", string(content), err)
	}

	// 2. Switch back to v1.7.0 in milliseconds
	if _, err := bm.UseVersion("terraform", "v1.7.0"); err != nil {
		t.Fatalf("UseVersion v1.7.0 failed: %v", err)
	}
	content17, err := os.ReadFile(activeBinPath)
	if err != nil || string(content17) != "#!/bin/sh\necho 'Terraform v1.7.0'\n" {
		t.Fatalf("expected active binary to be v1.7.0 after UseVersion, got %q", string(content17))
	}

	// 3. Toggle Global Binaries OFF and verify operations are blocked transparently
	if err := v.Config.SetFeature("global_binaries", false); err != nil {
		t.Fatal(err)
	}
	if _, err := bm.UseVersion("terraform", "v1.8.0"); err == nil {
		t.Fatalf("expected UseVersion to fail when global_binaries module is OFF")
	}
}
