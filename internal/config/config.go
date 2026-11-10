package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FeatureName identifies each toggleable capability in Permafrost.
type FeatureName string

const (
	FeatureSafeSwitch      FeatureName = "safe_switch"
	FeatureHooks           FeatureName = "hooks"
	FeatureTimeTravelDiff  FeatureName = "time_travel_diff"
	FeatureZeroKnowledge   FeatureName = "zero_knowledge_encryption"
	FeatureTeleport        FeatureName = "teleport"
	FeatureGlobalBinaries  FeatureName = "global_binaries"
)

// FeatureInfo provides metadata for transparent display in CLI and TUI.
type FeatureInfo struct {
	Key         FeatureName `json:"key"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Enabled     bool        `json:"enabled"`
}

// FeatureToggles holds the ON/OFF state of all modular capabilities.
type FeatureToggles struct {
	SafeSwitch              bool `json:"safe_switch" yaml:"safe_switch"`
	Hooks                   bool `json:"hooks" yaml:"hooks"`
	TimeTravelDiff          bool `json:"time_travel_diff" yaml:"time_travel_diff"`
	ZeroKnowledgeEncryption bool `json:"zero_knowledge_encryption" yaml:"zero_knowledge_encryption"`
	Teleport                bool `json:"teleport" yaml:"teleport"`
	GlobalBinaries          bool `json:"global_binaries" yaml:"global_binaries"`
}

// ChunkingConfig controls the Content-Defined Chunking (CDC) and compression parameters.
type ChunkingConfig struct {
	MinChunkSize     int    `json:"min_chunk_size" yaml:"min_chunk_size"`         // Default: 32 KB
	AvgChunkSize     int    `json:"avg_chunk_size" yaml:"avg_chunk_size"`         // Default: 128 KB
	MaxChunkSize     int    `json:"max_chunk_size" yaml:"max_chunk_size"`         // Default: 512 KB
	CompressionLevel string `json:"compression_level" yaml:"compression_level"`   // fast, default, best
	MaxAutoStashes   int    `json:"max_auto_stashes" yaml:"max_auto_stashes"`     // Max Safe Switch stashes per project
}

// CloudBackendConfig stores optional remote target configuration (GitHub Releases or S3).
type CloudBackendConfig struct {
	ActiveProvider string `json:"active_provider" yaml:"active_provider"` // "none", "github", "s3"
	GitHubRepo     string `json:"github_repo" yaml:"github_repo"`         // e.g. "amglogicalis/permafrost-vault"
	ChunkLimitMB   int    `json:"chunk_limit_mb" yaml:"chunk_limit_mb"`   // e.g. 100 MB (< 2GB GitHub limit)
	S3Endpoint     string `json:"s3_endpoint" yaml:"s3_endpoint"`
	S3Bucket       string `json:"s3_bucket" yaml:"s3_bucket"`
	S3Region       string `json:"s3_region" yaml:"s3_region"`
	KeySaltHex     string `json:"key_salt_hex,omitempty" yaml:"key_salt_hex,omitempty"`
	KeyCheckHex    string `json:"key_check_hex,omitempty" yaml:"key_check_hex,omitempty"`
}

// GlobalConfig represents ~/.permafrost/config.json.
type GlobalConfig struct {
	Version         string                    `json:"version" yaml:"version"`
	VaultDir        string                    `json:"vault_dir" yaml:"vault_dir"`
	Features        FeatureToggles            `json:"features" yaml:"features"`
	Chunking        ChunkingConfig            `json:"chunking" yaml:"chunking"`
	Cloud           CloudBackendConfig        `json:"cloud" yaml:"cloud"`
	ProjectFeatures map[string]FeatureToggles `json:"project_features,omitempty" yaml:"project_features,omitempty"`
	DefaultIgnores  []string                  `json:"default_ignores" yaml:"default_ignores"`
}

// DefaultVaultDir resolves the central vault path (~/.permafrost by default, or PFROST_VAULT_DIR).
func DefaultVaultDir() (string, error) {
	if envDir := strings.TrimSpace(os.Getenv("PFROST_VAULT_DIR")); envDir != "" {
		return filepath.Clean(envDir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".permafrost"), nil
}

// NewDefaultConfig creates a clean default configuration with sensible defaults.
func NewDefaultConfig(vaultDir string) *GlobalConfig {
	return &GlobalConfig{
		Version:  "1.0.0",
		VaultDir: vaultDir,
		Features: FeatureToggles{
			SafeSwitch:              true,
			Hooks:                   true,
			TimeTravelDiff:          true,
			ZeroKnowledgeEncryption: true,
			Teleport:                true,
			GlobalBinaries:          true,
		},
		Chunking: ChunkingConfig{
			MinChunkSize:     32 * 1024,  // 32 KB
			AvgChunkSize:     128 * 1024, // 128 KB
			MaxChunkSize:     512 * 1024, // 512 KB
			CompressionLevel: "default",
			MaxAutoStashes:   15,
		},
		Cloud: CloudBackendConfig{
			ActiveProvider: "none",
			ChunkLimitMB:   100,
		},
		ProjectFeatures: make(map[string]FeatureToggles),
		DefaultIgnores: []string{
			".permafrost",
			".permafrost-test",
		},
	}
}

// LoadOrInit loads the configuration from the vault directory or creates it if missing.
func LoadOrInit(vaultDir string) (*GlobalConfig, error) {
	if vaultDir == "" {
		var err error
		vaultDir, err = DefaultVaultDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(vaultDir, 0755); err != nil {
		return nil, fmt.Errorf("creating vault directory %s: %w", vaultDir, err)
	}

	cfgPath := filepath.Join(vaultDir, "config.json")
	data, err := os.ReadFile(cfgPath)
	if os.IsNotExist(err) {
		cfg := NewDefaultConfig(vaultDir)
		if saveErr := cfg.Save(); saveErr != nil {
			return nil, saveErr
		}
		return cfg, nil
	} else if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", cfgPath, err)
	}

	cfg := NewDefaultConfig(vaultDir)
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", cfgPath, err)
	}
	cfg.VaultDir = vaultDir
	if cfg.ProjectFeatures == nil {
		cfg.ProjectFeatures = make(map[string]FeatureToggles)
	}
	return cfg, nil
}

// Save writes the configuration atomically to ~/.permafrost/config.json.
func (c *GlobalConfig) Save() error {
	if err := os.MkdirAll(c.VaultDir, 0755); err != nil {
		return err
	}
	cfgPath := filepath.Join(c.VaultDir, "config.json")
	tmpPath := cfgPath + ".tmp"

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, cfgPath)
}

// ListFeatures returns the transparent list of all features and their current status.
func (c *GlobalConfig) ListFeatures() []FeatureInfo {
	return []FeatureInfo{
		{
			Key:         FeatureSafeSwitch,
			Title:       "Safe Switch (Auto-Stash)",
			Description: "Crea automáticamente un snapshot de respaldo si detecta cambios sin guardar antes de cambiar de versión.",
			Enabled:     c.Features.SafeSwitch,
		},
		{
			Key:         FeatureHooks,
			Title:       "Hooks Pre-Freeze / Post-Switch",
			Description: "Ejecuta scripts de .permafrost.yaml antes de congelar o después de cambiar de versión con auditoría previa.",
			Enabled:     c.Features.Hooks,
		},
		{
			Key:         FeatureTimeTravelDiff,
			Title:       "Time-Travel Visual Diff",
			Description: "Muestra el resumen visual de archivos creados, modificados y eliminados antes de cambiar de versión.",
			Enabled:     c.Features.TimeTravelDiff,
		},
		{
			Key:         FeatureZeroKnowledge,
			Title:       "Zero-Knowledge Cloud Encryption",
			Description: "Cifra todos los bloques y manifiestos localmente (XChaCha20-Poly1305 / AES-256-GCM) antes de subirlos a la nube.",
			Enabled:     c.Features.ZeroKnowledgeEncryption,
		},
		{
			Key:         FeatureTeleport,
			Title:       "Teleport (Instant Environment Share)",
			Description: "Permite empaquetar y compartir estados exactos de proyectos mediante códigos o paquetes efímeros.",
			Enabled:     c.Features.Teleport,
		},
		{
			Key:         FeatureGlobalBinaries,
			Title:       "Permafrost Global Binaries",
			Description: "Gestiona y cambia versiones de ejecutables sueltos (terraform, node, etc.) en ~/.permafrost/bin.",
			Enabled:     c.Features.GlobalBinaries,
		},
	}
}

// SetFeature toggles a feature ON or OFF and saves the configuration.
func (c *GlobalConfig) SetFeature(name string, enabled bool) error {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.ReplaceAll(normalized, "-", "_")

	switch FeatureName(normalized) {
	case FeatureSafeSwitch, "safeswitch":
		c.Features.SafeSwitch = enabled
	case FeatureHooks, "hook":
		c.Features.Hooks = enabled
	case FeatureTimeTravelDiff, "diff", "timetraveldiff":
		c.Features.TimeTravelDiff = enabled
	case FeatureZeroKnowledge, "encryption", "crypto", "zeroknowledge":
		c.Features.ZeroKnowledgeEncryption = enabled
	case FeatureTeleport, "share":
		c.Features.Teleport = enabled
	case FeatureGlobalBinaries, "binaries", "bin":
		c.Features.GlobalBinaries = enabled
	default:
		return fmt.Errorf("módulo desconocido %q (válidos: safe_switch, hooks, time_travel_diff, zero_knowledge_encryption, teleport, global_binaries)", name)
	}
	return c.Save()
}
