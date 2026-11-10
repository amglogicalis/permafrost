package engine

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/amglogicalis/permafrost/internal/vault"
)

func TestFastCDCDeduplicationAndHotSwap(t *testing.T) {
	vaultDir := t.TempDir()
	projectDir := t.TempDir()

	v, err := vault.Open(vaultDir)
	if err != nil {
		t.Fatalf("failed to open vault: %v", err)
	}
	eng, err := New(v)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	// Create deterministic 2 MB pseudo-random payload + source files
	baseBlob := make([]byte, 2*1024*1024)
	rng := rand.Reader
	if _, err := rng.Read(baseBlob); err != nil {
		t.Fatalf("failed to generate random bytes: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(projectDir, "infra"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "infra", "main.tf"), []byte(`resource "aws_s3_bucket" "app" { bucket = "v1-prod" }`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "bundle.bin"), baseBlob, 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Freeze v1
	m1, err := eng.Freeze(projectDir, FreezeOptions{
		Tag:     "v1.0.0",
		Message: "Initial stable state",
	})
	if err != nil {
		t.Fatalf("Freeze v1 failed: %v", err)
	}
	if m1.Stats.TotalFiles != 2 {
		t.Fatalf("expected 2 files in v1, got %d", m1.Stats.TotalFiles)
	}
	if m1.Stats.NewUniqueChunks == 0 {
		t.Fatalf("expected new unique chunks > 0 in v1")
	}

	// 2. Modify only 64 bytes in the middle of the 2MB binary + update main.tf + add extra.txt
	modifiedBlob := make([]byte, len(baseBlob))
	copy(modifiedBlob, baseBlob)
	copy(modifiedBlob[512*1024:], []byte("PERMAFROST_DELTA_MODIFICATION_IN_MIDDLE_OF_LARGE_BINARY_FILE_V2!"))

	if err := os.WriteFile(filepath.Join(projectDir, "bundle.bin"), modifiedBlob, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "infra", "main.tf"), []byte(`resource "aws_s3_bucket" "app" { bucket = "v2-staging" }`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "extra.txt"), []byte("temporary file in v2"), 0644); err != nil {
		t.Fatal(err)
	}

	// 3. Freeze v2
	m2, err := eng.Freeze(projectDir, FreezeOptions{
		Tag:     "v2.0.0",
		Message: "Modified binary + updated terraform + extra file",
	})
	if err != nil {
		t.Fatalf("Freeze v2 failed: %v", err)
	}

	// Verify FastCDC block deduplication: most chunks of the 2MB binary must be reused!
	if m2.Stats.ReusedChunks == 0 {
		t.Fatalf("expected FastCDC to reuse unchanged chunks in 2MB binary, got 0 reused chunks")
	}
	if m2.Stats.NewBytesStored >= m1.Stats.NewBytesStored/2 {
		t.Fatalf("expected v2 new bytes stored (%d) to be much smaller than v1 (%d)", m2.Stats.NewBytesStored, m1.Stats.NewBytesStored)
	}
	t.Logf("Deduplication check passed: v1 stored=%d B, v2 new stored=%d B (reused %d/%d chunks, saved %.1f%%)",
		m1.Stats.NewBytesStored, m2.Stats.NewBytesStored, m2.Stats.ReusedChunks, m2.Stats.TotalChunks, m2.Stats.SpaceSavedPercent)

	// 4. Hot-swap back to v1 and verify byte-for-byte restoration + removal of extra.txt
	sw1, err := eng.Restore(projectDir, m1, nil, true)
	if err != nil {
		t.Fatalf("Restore to v1 failed: %v", err)
	}
	if sw1.FilesRemoved != 1 {
		t.Fatalf("expected 1 untracked file (extra.txt) removed when switching to v1, got %d", sw1.FilesRemoved)
	}

	restoredBlob, err := os.ReadFile(filepath.Join(projectDir, "bundle.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restoredBlob, baseBlob) {
		t.Fatalf("restored bundle.bin does not match v1 byte-for-byte!")
	}

	// 5. Repeat restore to v1 immediately: all files should be skipped as unchanged (0 disk writes)
	swRepeat, err := eng.Restore(projectDir, m1, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if swRepeat.FilesSkippedUnchanged != 2 || swRepeat.BytesWritten != 0 {
		t.Fatalf("expected 2 files skipped unchanged and 0 bytes written on idempotent switch, got skipped=%d written=%d",
			swRepeat.FilesSkippedUnchanged, swRepeat.BytesWritten)
	}

	// 6. Hot-swap back to v2 and verify byte-for-byte restoration
	if _, err := eng.Restore(projectDir, m2, nil, true); err != nil {
		t.Fatalf("Restore to v2 failed: %v", err)
	}
	restoredV2Blob, err := os.ReadFile(filepath.Join(projectDir, "bundle.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restoredV2Blob, modifiedBlob) {
		t.Fatalf("restored bundle.bin does not match v2 byte-for-byte!")
	}

	// 7. Test Garbage Collection after deleting v2
	if _, err := v.DeleteManifest(m2.ProjectID, m2.ID, false); err != nil {
		t.Fatalf("DeleteManifest v2 failed: %v", err)
	}
	gcRes, err := v.GarbageCollect()
	if err != nil {
		t.Fatalf("GarbageCollect failed: %v", err)
	}
	if gcRes.OrphanBlobsFreed == 0 {
		t.Fatalf("expected orphan blobs from v2 to be freed by GC, got 0")
	}

	// Verify v1 is still 100% restorable after GC!
	if _, err := eng.Restore(projectDir, m1, nil, true); err != nil {
		t.Fatalf("Restore v1 after GC failed: %v", err)
	}
}
