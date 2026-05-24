package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const validConfigYAML = `
host: "127.0.0.1"
port: 39998
virtual_api_keys:
  - "sk-test-1"
providers:
  - name: test
    type: openai
    base_url: "https://api.example.com/v1"
    api_key: "sk-upstream"
    models:
      "model-a": "real-model-a"
`

func TestWatcher_ReloadOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(validConfigYAML), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var reloaded bool
	var reloadedCfg *Config

	onReload := func(c *Config) {
		mu.Lock()
		reloaded = true
		reloadedCfg = c
		mu.Unlock()
	}

	w, err := NewWatcher(path, cfg, onReload)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Update the config file
	updatedYAML := `
host: "127.0.0.1"
port: 39998
virtual_api_keys:
  - "sk-test-1"
  - "sk-test-2"
providers:
  - name: test
    type: openai
    base_url: "https://api.example.com/v1"
    api_key: "sk-upstream"
    models:
      "model-a": "real-model-a"
      "model-b": "real-model-b"
`
	time.Sleep(100 * time.Millisecond)
	os.WriteFile(path, []byte(updatedYAML), 0o644)

	time.Sleep(1 * time.Second)

	mu.Lock()
	defer mu.Unlock()

	if !reloaded {
		t.Fatal("onReload was not called")
	}
	if len(reloadedCfg.VirtualAPIKeys) != 2 {
		t.Errorf("expected 2 api keys, got %d", len(reloadedCfg.VirtualAPIKeys))
	}
}

func TestWatcher_InvalidYAMLKeepsOldConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(validConfigYAML), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var reloadCount int

	onReload := func(_ *Config) {
		mu.Lock()
		reloadCount++
		mu.Unlock()
	}

	w, err := NewWatcher(path, cfg, onReload)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	time.Sleep(100 * time.Millisecond)
	os.WriteFile(path, []byte("{{invalid yaml"), 0o644)

	time.Sleep(1 * time.Second)

	mu.Lock()
	defer mu.Unlock()

	if reloadCount != 0 {
		t.Errorf("onReload should not have been called for invalid config, got %d calls", reloadCount)
	}
}

func TestWatcher_Debounce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(validConfigYAML), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var reloadCount int

	onReload := func(_ *Config) {
		mu.Lock()
		reloadCount++
		mu.Unlock()
	}

	w, err := NewWatcher(path, cfg, onReload)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	time.Sleep(100 * time.Millisecond)

	for i := 0; i < 5; i++ {
		os.WriteFile(path, []byte(validConfigYAML), 0o644)
		time.Sleep(50 * time.Millisecond)
	}

	time.Sleep(1 * time.Second)

	mu.Lock()
	defer mu.Unlock()

	if reloadCount > 1 {
		t.Errorf("expected at most 1 reload due to debounce, got %d", reloadCount)
	}
}

func TestWatcher_ImmutableFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(validConfigYAML), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var reloaded bool
	onReload := func(_ *Config) {
		mu.Lock()
		reloaded = true
		mu.Unlock()
	}

	w, err := NewWatcher(path, cfg, onReload)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	changedPortYAML := `
host: "127.0.0.1"
port: 12345
virtual_api_keys:
  - "sk-test-1"
providers:
  - name: test
    type: openai
    base_url: "https://api.example.com/v1"
    api_key: "sk-upstream"
    models:
      "model-a": "real-model-a"
`
	time.Sleep(100 * time.Millisecond)
	os.WriteFile(path, []byte(changedPortYAML), 0o644)

	time.Sleep(1 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if !reloaded {
		t.Error("expected reload to succeed even with immutable field change")
	}
}
