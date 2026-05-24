package config

import (
	"os"
	"path/filepath"
	"testing"
)

const validYAML = `
host: "127.0.0.1"
port: 39998
virtual_api_keys:
  - "sk-test-1"
  - "sk-test-2"
providers:
  - name: test-provider
    type: openai
    base_url: "https://api.example.com/v1"
    api_key: "sk-upstream"
    timeout: 1800s
    models:
      "test-model": "real-model"
database:
  driver: sqlite
  dsn: "/tmp/test.db"
log_level: debug
`

func TestLoad_ValidConfig(t *testing.T) {
	path := writeTemp(t, validYAML)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Host != "127.0.0.1" {
		t.Errorf("host = %q, want %q", cfg.Host, "127.0.0.1")
	}
	if cfg.Port != 39998 {
		t.Errorf("port = %d, want %d", cfg.Port, 39998)
	}
	if len(cfg.VirtualAPIKeys) != 2 {
		t.Errorf("len(virtual_api_keys) = %d, want 2", len(cfg.VirtualAPIKeys))
	}
	if len(cfg.Providers) != 1 {
		t.Errorf("len(providers) = %d, want 1", len(cfg.Providers))
	}
	if cfg.Providers[0].Models["test-model"] != "real-model" {
		t.Errorf("model mapping: got %q, want %q", cfg.Providers[0].Models["test-model"], "real-model")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "debug")
	}
}

func TestLoad_SyntaxError(t *testing.T) {
	path := writeTemp(t, "{{invalid yaml")

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoad_MissingFields(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "no api keys",
			yaml: `
port: 8080
providers:
  - name: p
    type: openai
    base_url: http://x
    api_key: k
    models:
      a: b
`,
		},
		{
			name: "no providers",
			yaml: `
virtual_api_keys: ["k"]
`,
		},
		{
			name: "provider missing base_url",
			yaml: `
virtual_api_keys: ["k"]
providers:
  - name: p
    type: openai
    api_key: k
    models:
      a: b
`,
		},
		{
			name: "provider unsupported type",
			yaml: `
virtual_api_keys: ["k"]
providers:
  - name: p
    type: anthropic
    base_url: http://x
    api_key: k
    models:
      a: b
`,
		},
		{
			name: "duplicate provider name",
			yaml: `
virtual_api_keys: ["k"]
providers:
  - name: p
    type: openai
    base_url: http://x
    api_key: k
    models:
      a: b
  - name: p
    type: openai
    base_url: http://y
    api_key: k
    models:
      c: d
`,
		},
		{
			name: "invalid port",
			yaml: `
port: 99999
virtual_api_keys: ["k"]
providers:
  - name: p
    type: openai
    base_url: http://x
    api_key: k
    models:
      a: b
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, tt.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected validation error for %s", tt.name)
			}
		})
	}
}

func TestLoad_Defaults(t *testing.T) {
	yaml := `
virtual_api_keys: ["k"]
providers:
  - name: p
    type: openai
    base_url: http://x
    api_key: k
    models:
      a: b
`
	path := writeTemp(t, yaml)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Host != "127.0.0.1" {
		t.Errorf("default host = %q, want %q", cfg.Host, "127.0.0.1")
	}
	if cfg.Port != 39998 {
		t.Errorf("default port = %d, want %d", cfg.Port, 39998)
	}
	if cfg.Database.Driver != "sqlite" {
		t.Errorf("default driver = %q, want %q", cfg.Database.Driver, "sqlite")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("default log_level = %q, want %q", cfg.LogLevel, "info")
	}
}

func TestEnsureConfigDir(t *testing.T) {
	if err := EnsureConfigDir(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dir := filepath.Join(os.Getenv("HOME"), ".wing-ai-proxy")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("config dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("config path is not a directory")
	}
}

func TestDefaultConfigPath(t *testing.T) {
	p := DefaultConfigPath()
	if p == "" {
		t.Fatal("DefaultConfigPath returned empty string")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}
