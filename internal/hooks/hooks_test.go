package hooks

import (
	"os"
	"strings"
	"testing"

	"github.com/amglogicalis/permafrost/internal/vault"
)

func TestHooksLifecycleTrustAndToggle(t *testing.T) {
	vaultDir := t.TempDir()
	projDir := t.TempDir()

	v, err := vault.Open(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	runner := New(v)

	// 1. Initialize .permafrost.yaml template (auto-trusts it)
	yamlPath, err := runner.InitTemplate(projDir, false)
	if err != nil {
		t.Fatalf("InitTemplate failed: %v", err)
	}
	if _, err := os.Stat(yamlPath); err != nil {
		t.Fatalf("expected .permafrost.yaml to exist: %v", err)
	}

	// 2. Execute pre_freeze hook when enabled & trusted
	results, summary, err := runner.ExecuteStage(projDir, StagePreFreeze, "v1.0")
	if err != nil {
		t.Fatalf("ExecuteStage pre_freeze failed: %v", err)
	}
	if len(results) != 1 || !results[0].Success || !strings.Contains(results[0].Output, "[Permafrost Hook]") {
		t.Fatalf("unexpected hook execution result: %+v (%s)", results, summary)
	}

	// 3. Modify .permafrost.yaml without re-trusting -> must block execution transparently
	untrustedYAML := `enabled: true
hooks:
  pre_freeze:
    - echo "Modified command"
`
	if err := os.WriteFile(yamlPath, []byte(untrustedYAML), 0644); err != nil {
		t.Fatal(err)
	}
	resultsUntrusted, notice, err := runner.ExecuteStage(projDir, StagePreFreeze, "v1.0")
	if err != nil || len(resultsUntrusted) != 0 || !strings.Contains(notice, "Hooks No Autorizados") {
		t.Fatalf("expected untrusted hook to be blocked with transparent notice, got results=%v notice=%q err=%v",
			resultsUntrusted, notice, err)
	}

	// 4. Trust the modified .permafrost.yaml -> should now run
	if _, err := runner.Trust(projDir); err != nil {
		t.Fatalf("Trust failed: %v", err)
	}
	resultsTrusted, _, err := runner.ExecuteStage(projDir, StagePreFreeze, "v1.0")
	if err != nil || len(resultsTrusted) != 1 || !strings.Contains(resultsTrusted[0].Output, "Modified command") {
		t.Fatalf("expected trusted hook to run, got %+v (%v)", resultsTrusted, err)
	}

	// 5. Disable global Hooks feature -> must skip transparently
	if err := v.Config.SetFeature("hooks", false); err != nil {
		t.Fatal(err)
	}
	resultsOff, noticeOff, err := runner.ExecuteStage(projDir, StagePreFreeze, "v1.0")
	if err != nil || len(resultsOff) != 0 || !strings.Contains(noticeOff, "[Hooks: OFF]") {
		t.Fatalf("expected disabled hook to skip with [Hooks: OFF], got %v %q", resultsOff, noticeOff)
	}
}
