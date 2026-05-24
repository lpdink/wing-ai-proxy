package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level application configuration.
type Config struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`

	VirtualAPIKeys []string         `yaml:"virtual_api_keys"`
	Providers      []ProviderConfig `yaml:"providers"`

	Database DatabaseConfig `yaml:"database"`

	LogLevel string `yaml:"log_level"`
}

// ProviderConfig defines an upstream LLM provider.
type ProviderConfig struct {
	Name    string            `yaml:"name"`
	Type    string            `yaml:"type"`
	BaseURL string            `yaml:"base_url"`
	APIKey  string            `yaml:"api_key"`
	Timeout time.Duration     `yaml:"timeout"`
	Models  map[string]string `yaml:"models"` // show_name → real_name
}

// DatabaseConfig defines the audit database settings.
type DatabaseConfig struct {
	Driver string `yaml:"driver"` // "sqlite" (default) or "postgres" (future)
	DSN    string `yaml:"dsn"`    // file path for sqlite, connection string for postgres
}

// DefaultConfigPath returns ~/.wing-ai-proxy/config.yaml.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".wing-ai-proxy", "config.yaml")
}

// DefaultDBPath returns ~/.wing-ai-proxy/sqlite.db.
func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".wing-ai-proxy", "sqlite.db")
}

// Load reads and parses a YAML configuration file, applying validation.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// EnsureConfigDir creates the config directory if it doesn't exist.
func EnsureConfigDir() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(home, ".wing-ai-proxy")
	return os.MkdirAll(dir, 0o755)
}

func applyDefaults(cfg *Config) {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 39998
	}
	if cfg.Database.Driver == "" {
		cfg.Database.Driver = "sqlite"
	}
	if cfg.Database.DSN == "" {
		cfg.Database.DSN = DefaultDBPath()
	}
	// Expand ~ in DSN path
	cfg.Database.DSN = expandHomePath(cfg.Database.DSN)
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].Timeout == 0 {
			cfg.Providers[i].Timeout = 30 * time.Minute
		}
	}
}

// expandHomePath replaces leading ~ with the user's home directory.
func expandHomePath(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}

func validate(cfg *Config) error {
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("config: port must be between 1 and 65535, got %d", cfg.Port)
	}

	if len(cfg.VirtualAPIKeys) == 0 {
		return fmt.Errorf("config: virtual_api_keys must not be empty")
	}
	for i, key := range cfg.VirtualAPIKeys {
		if key == "" {
			return fmt.Errorf("config: virtual_api_keys[%d] is empty", i)
		}
	}

	if len(cfg.Providers) == 0 {
		return fmt.Errorf("config: providers must not be empty")
	}

	names := make(map[string]bool)
	for i, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("config: providers[%d].name is required", i)
		}
		if names[p.Name] {
			return fmt.Errorf("config: duplicate provider name %q", p.Name)
		}
		names[p.Name] = true

		if p.Type == "" {
			return fmt.Errorf("config: providers[%d] (%s): type is required", i, p.Name)
		}
		if p.Type != "openai" {
			return fmt.Errorf("config: providers[%d] (%s): unsupported type %q", i, p.Name, p.Type)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("config: providers[%d] (%s): base_url is required", i, p.Name)
		}
		if p.APIKey == "" {
			return fmt.Errorf("config: providers[%d] (%s): api_key is required", i, p.Name)
		}
		if len(p.Models) == 0 {
			return fmt.Errorf("config: providers[%d] (%s): models must not be empty", i, p.Name)
		}
		for showName, realName := range p.Models {
			if showName == "" {
				return fmt.Errorf("config: providers[%d] (%s): empty show_name in models", i, p.Name)
			}
			if realName == "" {
				return fmt.Errorf("config: providers[%d] (%s): empty real_name for model %q", i, p.Name, showName)
			}
		}
	}

	return nil
}
