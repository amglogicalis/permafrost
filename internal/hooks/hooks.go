package hooks

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/amglogicalis/permafrost/internal/vault"
)

// Stage identifies which lifecycle hook is being triggered.
type Stage string

const (
	StagePreFreeze  Stage = "pre_freeze"
	StagePostFreeze Stage = "post_freeze"
	StagePreSwitch  Stage = "pre_switch"
	StagePostSwitch Stage = "post_switch"
)

// ConfigFile represents the contents of a project's optional .permafrost.yaml file.
type ConfigFile struct {
	Enabled     *bool             `json:"enabled,omitempty"`
	PreFreeze   []string          `json:"pre_freeze,omitempty"`
	PostFreeze  []string          `json:"post_freeze,omitempty"`
	PreSwitch   []string          `json:"pre_switch,omitempty"`
	PostSwitch  []string          `json:"post_switch,omitempty"`
	Environment map[string]string `json:"env,omitempty"`
	FileHash    string            `json:"file_hash"`
	FilePath    string            `json:"file_path"`
}

// CommandExecution records the transparent result of a single hook command.
type CommandExecution struct {
	Stage      Stage  `json:"stage"`
	Command    string `json:"command"`
	Output     string `json:"output"`
	DurationMs int64  `json:"duration_ms"`
	Success    bool   `json:"success"`
}

// TrustStore persists authorized .permafrost.yaml SHA-256 hashes in ~/.permafrost/trusted_hooks.json.
type TrustStore struct {
	TrustedHashes map[string]string `json:"trusted_hashes"` // projectPath -> sha256
}

// Runner manages discovery, trust verification, and transparent execution of .permafrost.yaml hooks.
type Runner struct {
	Vault *vault.Vault
}

// New creates a Hooks Runner backed by the central Vault.
func New(v *vault.Vault) *Runner {
	return &Runner{Vault: v}
}

// IsGloballyEnabled returns whether the global Hooks module is ON.
func (r *Runner) IsGloballyEnabled() bool {
	return r.Vault.Config.Features.Hooks
}

// LoadProjectHooks reads and parses .permafrost.yaml (if present) in projectPath.
func (r *Runner) LoadProjectHooks(projectPath string) (*ConfigFile, error) {
	absRoot, err := vault.NormalizeProjectPath(projectPath)
	if err != nil {
		return nil, err
	}
	yamlPath := filepath.Join(absRoot, ".permafrost.yaml")
	data, err := os.ReadFile(yamlPath)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(data)
	hashHex := hex.EncodeToString(sum[:])

	cfg, err := parseHooksYAML(string(data))
	if err != nil {
		return nil, fmt.Errorf("error leyendo .permafrost.yaml: %w", err)
	}
	cfg.FileHash = hashHex
	cfg.FilePath = yamlPath
	return cfg, nil
}

// IsTrusted checks if the current .permafrost.yaml hash is authorized in ~/.permafrost/trusted_hooks.json.
func (r *Runner) IsTrusted(projectPath string, cfg *ConfigFile) bool {
	if cfg == nil {
		return false
	}
	absRoot, err := vault.NormalizeProjectPath(projectPath)
	if err != nil {
		return false
	}
	ts := r.loadTrustStore()
	return ts.TrustedHashes[absRoot] == cfg.FileHash
}

// Trust authorizes the current .permafrost.yaml file in projectPath.
func (r *Runner) Trust(projectPath string) (*ConfigFile, error) {
	absRoot, err := vault.NormalizeProjectPath(projectPath)
	if err != nil {
		return nil, err
	}
	cfg, err := r.LoadProjectHooks(absRoot)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, fmt.Errorf("no existe ningún archivo .permafrost.yaml en %s (usa 'pfrost hooks init' para crear uno)", absRoot)
	}

	ts := r.loadTrustStore()
	ts.TrustedHashes[absRoot] = cfg.FileHash
	if err := r.saveTrustStore(ts); err != nil {
		return nil, err
	}
	r.Vault.AppendAuditLog("HOOKS_TRUSTED", fmt.Sprintf("Project=%s Hash=%s", absRoot, cfg.FileHash[:12]))
	return cfg, nil
}

// Revoke removes trust for .permafrost.yaml in projectPath.
func (r *Runner) Revoke(projectPath string) error {
	absRoot, err := vault.NormalizeProjectPath(projectPath)
	if err != nil {
		return err
	}
	ts := r.loadTrustStore()
	delete(ts.TrustedHashes, absRoot)
	if err := r.saveTrustStore(ts); err != nil {
		return err
	}
	r.Vault.AppendAuditLog("HOOKS_REVOKED", fmt.Sprintf("Project=%s", absRoot))
	return nil
}

// InitTemplate creates a clean, well-documented .permafrost.yaml file and trusts it automatically.
func (r *Runner) InitTemplate(projectPath string, overwrite bool) (string, error) {
	absRoot, err := vault.NormalizeProjectPath(projectPath)
	if err != nil {
		return "", err
	}
	yamlPath := filepath.Join(absRoot, ".permafrost.yaml")
	if !overwrite {
		if _, err := os.Stat(yamlPath); err == nil {
			return yamlPath, fmt.Errorf(".permafrost.yaml ya existe en %s", absRoot)
		}
	}

	template := `# Configuración de Hooks de Permafrost (pfrost)
# Totalmente transparente, activable y desactivable con: pfrost feature hooks on|off
enabled: true

hooks:
  pre_freeze:
    - echo "[Permafrost Hook] Preparando proyecto antes de congelar..."
  post_freeze:
    - echo "[Permafrost Hook] Snapshot congelado correctamente."
  pre_switch:
    - echo "[Permafrost Hook] Preparando cambio de versión..."
  post_switch:
    - echo "[Permafrost Hook] Versión restaurada y lista."
`
	if err := os.WriteFile(yamlPath, []byte(template), 0644); err != nil {
		return "", err
	}
	_, _ = r.Trust(absRoot)
	return yamlPath, nil
}

// ExecuteStage runs all commands for the given stage if Hooks are enabled and trusted.
func (r *Runner) ExecuteStage(projectPath string, stage Stage, targetTag string) ([]CommandExecution, string, error) {
	cfg, err := r.LoadProjectHooks(projectPath)
	if err != nil || cfg == nil {
		return nil, "", err
	}

	cmds := cfg.CommandsForStage(stage)
	if len(cmds) == 0 {
		return nil, "", nil
	}

	// Check global and local enabled state
	if !r.IsGloballyEnabled() {
		notice := fmt.Sprintf("ℹ [Hooks: OFF] Se omitieron %d comando(s) de %s en .permafrost.yaml", len(cmds), stage)
		return nil, notice, nil
	}
	if cfg.Enabled != nil && !*cfg.Enabled {
		notice := fmt.Sprintf("ℹ [Hooks Local: OFF] Se omitieron %d comando(s) de %s en .permafrost.yaml", len(cmds), stage)
		return nil, notice, nil
	}

	// Verify trust before running shell commands
	if !r.IsTrusted(projectPath, cfg) {
		notice := fmt.Sprintf("⚠ [Hooks No Autorizados] .permafrost.yaml detectado pero su firma (%s) no está autorizada. Ejecuta 'pfrost hooks trust' para autorizarlo.", cfg.FileHash[:10])
		return nil, notice, nil
	}

	var results []CommandExecution
	for _, cmdStr := range cmds {
		start := time.Now()
		var c *exec.Cmd
		if runtime.GOOS == "windows" {
			c = exec.Command("cmd.exe", "/C", cmdStr)
		} else {
			c = exec.Command("sh", "-c", cmdStr)
		}
		c.Dir = projectPath
		c.Env = append(os.Environ(),
			"PFROST_STAGE="+string(stage),
			"PFROST_TARGET_TAG="+targetTag,
			"PFROST_PROJECT_PATH="+projectPath,
		)

		var outBuf bytes.Buffer
		c.Stdout = &outBuf
		c.Stderr = &outBuf

		runErr := c.Run()
		dur := time.Since(start).Milliseconds()
		outText := strings.TrimSpace(outBuf.String())

		execRes := CommandExecution{
			Stage:      stage,
			Command:    cmdStr,
			Output:     outText,
			DurationMs: dur,
			Success:    runErr == nil,
		}
		results = append(results, execRes)

		r.Vault.AppendAuditLog("HOOK_EXECUTED", fmt.Sprintf("Stage=%s Cmd=%q Success=%v Duration=%dms",
			stage, cmdStr, runErr == nil, dur))

		if runErr != nil {
			return results, "", fmt.Errorf("hook %s falló en comando %q: %v (salida: %s)", stage, cmdStr, runErr, outText)
		}
	}

	summary := fmt.Sprintf("⚡ [Hooks %s] %d comando(s) ejecutado(s) con éxito", stage, len(results))
	return results, summary, nil
}

// CommandsForStage returns the configured command list for the requested lifecycle stage.
func (c *ConfigFile) CommandsForStage(stage Stage) []string {
	switch stage {
	case StagePreFreeze:
		return c.PreFreeze
	case StagePostFreeze:
		return c.PostFreeze
	case StagePreSwitch:
		return c.PreSwitch
	case StagePostSwitch:
		return c.PostSwitch
	default:
		return nil
	}
}

func (r *Runner) trustStorePath() string {
	return filepath.Join(r.Vault.RootDir, "trusted_hooks.json")
}

func (r *Runner) loadTrustStore() *TrustStore {
	ts := &TrustStore{TrustedHashes: make(map[string]string)}
	data, err := os.ReadFile(r.trustStorePath())
	if err == nil {
		_ = json.Unmarshal(data, ts)
	}
	if ts.TrustedHashes == nil {
		ts.TrustedHashes = make(map[string]string)
	}
	return ts
}

func (r *Runner) saveTrustStore(ts *TrustStore) error {
	data, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.trustStorePath(), data, 0600)
}

func parseHooksYAML(content string) (*ConfigFile, error) {
	cfg := &ConfigFile{
		Environment: make(map[string]string),
	}

	// Support JSON syntax automatically if file starts with '{'
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	var currentSection string
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		rawLine := sc.Text()
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "enabled:") {
			val := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "enabled:")))
			b := val == "true" || val == "on" || val == "yes" || val == "1"
			cfg.Enabled = &b
			continue
		}

		switch strings.TrimSuffix(line, ":") {
		case "hooks":
			continue
		case "pre_freeze":
			currentSection = "pre_freeze"
			continue
		case "post_freeze":
			currentSection = "post_freeze"
			continue
		case "pre_switch":
			currentSection = "pre_switch"
			continue
		case "post_switch":
			currentSection = "post_switch"
			continue
		}

		if strings.HasPrefix(line, "-") {
			item := strings.TrimSpace(strings.TrimPrefix(line, "-"))
			if len(item) >= 2 && ((item[0] == '"' && item[len(item)-1] == '"') || (item[0] == '\'' && item[len(item)-1] == '\'')) {
				item = item[1 : len(item)-1]
			}
			if item == "" {
				continue
			}
			switch currentSection {
			case "pre_freeze":
				cfg.PreFreeze = append(cfg.PreFreeze, item)
			case "post_freeze":
				cfg.PostFreeze = append(cfg.PostFreeze, item)
			case "pre_switch":
				cfg.PreSwitch = append(cfg.PreSwitch, item)
			case "post_switch":
				cfg.PostSwitch = append(cfg.PostSwitch, item)
			}
		}
	}
	return cfg, nil
}
