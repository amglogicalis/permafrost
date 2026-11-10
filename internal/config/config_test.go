package config

import (
	"testing"
)

func TestFeatureTogglesPersistence(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadOrInit(dir)
	if err != nil {
		t.Fatalf("LoadOrInit failed: %v", err)
	}

	// Verify all features are ON by default
	for _, f := range cfg.ListFeatures() {
		if !f.Enabled {
			t.Fatalf("expected feature %s to be enabled by default", f.Key)
		}
	}

	// Toggle SafeSwitch and Teleport OFF
	if err := cfg.SetFeature("safe_switch", false); err != nil {
		t.Fatalf("SetFeature safe_switch failed: %v", err)
	}
	if err := cfg.SetFeature("teleport", false); err != nil {
		t.Fatalf("SetFeature teleport failed: %v", err)
	}

	// Reload from disk and verify state persisted
	reloaded, err := LoadOrInit(dir)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.Features.SafeSwitch {
		t.Fatalf("expected SafeSwitch to remain false after reload")
	}
	if reloaded.Features.Teleport {
		t.Fatalf("expected Teleport to remain false after reload")
	}
	if !reloaded.Features.ZeroKnowledgeEncryption {
		t.Fatalf("expected ZeroKnowledgeEncryption to remain true")
	}
}
