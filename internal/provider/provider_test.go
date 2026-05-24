package provider

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abiter/wing-ai-proxy/internal/config"
)

func TestOpenAIProvider_ChatCompletion(t *testing.T) {
	var receivedBody map[string]any
	var receivedAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	defer server.Close()

	cfg := config.ProviderConfig{
		Name:    "test",
		Type:    "openai",
		BaseURL: server.URL,
		APIKey:  "sk-upstream-key",
		Timeout: 10 * time.Second,
		Models:  map[string]string{"my-model": "real-model-v2"},
	}

	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}

	reqBody := `{"model":"my-model","messages":[{"role":"user","content":"hi"}]}`
	resp, err := p.ChatCompletion(context.Background(), []byte(reqBody), http.Header{})
	if err != nil {
		t.Fatalf("ChatCompletion error: %v", err)
	}
	defer resp.Body.Close()

	// Verify model was replaced
	if receivedBody["model"] != "real-model-v2" {
		t.Errorf("model not replaced: got %q, want %q", receivedBody["model"], "real-model-v2")
	}

	// Verify auth header
	if receivedAuth != "Bearer sk-upstream-key" {
		t.Errorf("auth header: got %q, want %q", receivedAuth, "Bearer sk-upstream-key")
	}

	// Verify response status
	if resp.StatusCode != 200 {
		t.Errorf("status code: got %d, want 200", resp.StatusCode)
	}
}

func TestOpenAIProvider_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(200)
	}))
	defer server.Close()

	cfg := config.ProviderConfig{
		Name:    "slow",
		Type:    "openai",
		BaseURL: server.URL,
		APIKey:  "k",
		Timeout: 100 * time.Millisecond,
		Models:  map[string]string{"m": "m"},
	}

	p, _ := NewProvider(cfg)
	_, err := p.ChatCompletion(context.Background(), []byte(`{"model":"m","messages":[]}`), http.Header{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestOpenAIProvider_CustomHeadersForwarded(t *testing.T) {
	var receivedHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	cfg := config.ProviderConfig{
		Name: "test", Type: "openai", BaseURL: server.URL,
		APIKey: "k", Timeout: 10 * time.Second,
		Models: map[string]string{"m": "m"},
	}

	p, _ := NewProvider(cfg)
	clientHeaders := http.Header{
		"X-Custom-Header": {"test-value"},
		"X-Trace-Id":      {"abc-123"},
	}

	resp, err := p.ChatCompletion(context.Background(), []byte(`{"model":"m"}`), clientHeaders)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if receivedHeaders.Get("X-Custom-Header") != "test-value" {
		t.Errorf("custom header not forwarded: got %q", receivedHeaders.Get("X-Custom-Header"))
	}
	if receivedHeaders.Get("X-Trace-Id") != "abc-123" {
		t.Errorf("trace id header not forwarded")
	}
}

func TestOpenAIProvider_AcceptEncodingNotForwarded(t *testing.T) {
	var receivedEncoding string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedEncoding = r.Header.Get("Accept-Encoding")
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	cfg := config.ProviderConfig{
		Name: "test", Type: "openai", BaseURL: server.URL,
		APIKey: "k", Timeout: 10 * time.Second,
		Models: map[string]string{"m": "m"},
	}

	p, _ := NewProvider(cfg)
	clientHeaders := http.Header{
		"Accept-Encoding": {"gzip, deflate"},
	}

	resp, err := p.ChatCompletion(context.Background(), []byte(`{"model":"m"}`), clientHeaders)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Accept-Encoding should NOT be forwarded from client;
	// Go's transport may set its own, but it should NOT be "gzip, deflate"
	if receivedEncoding == "gzip, deflate" {
		t.Errorf("client Accept-Encoding should not be forwarded, got: %q", receivedEncoding)
	}
}

func TestRegistry_Resolve(t *testing.T) {
	cfg1 := config.ProviderConfig{
		Name: "p1", Type: "openai", BaseURL: "http://p1",
		APIKey: "k", Timeout: time.Second,
		Models: map[string]string{"model-a": "real-a", "shared": "real-shared-p1"},
	}
	cfg2 := config.ProviderConfig{
		Name: "p2", Type: "openai", BaseURL: "http://p2",
		APIKey: "k", Timeout: time.Second,
		Models: map[string]string{"model-b": "real-b", "shared": "real-shared-p2"},
	}

	p1, _ := NewProvider(cfg1)
	p2, _ := NewProvider(cfg2)

	// Suppress expected warnings
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg := NewRegistry([]Provider{p1, p2})

	// Resolve unique model
	provider, realName, err := reg.Resolve("model-a")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "p1" {
		t.Errorf("wrong provider: got %q, want %q", provider.Name(), "p1")
	}
	if realName != "real-a" {
		t.Errorf("wrong real name: got %q, want %q", realName, "real-a")
	}

	// Resolve conflicting model (should use first provider)
	provider, _, err = reg.Resolve("shared")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "p1" {
		t.Errorf("conflict resolution: got provider %q, want %q", provider.Name(), "p1")
	}

	// Resolve non-existent model
	_, _, err = reg.Resolve("nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent model")
	}
}

func TestRegistry_AllModels(t *testing.T) {
	cfg := config.ProviderConfig{
		Name: "p", Type: "openai", BaseURL: "http://x",
		APIKey: "k", Timeout: time.Second,
		Models: map[string]string{"a": "ra", "b": "rb"},
	}
	p, _ := NewProvider(cfg)
	reg := NewRegistry([]Provider{p})

	models := reg.AllModels()
	if len(models) != 2 {
		t.Errorf("AllModels count: got %d, want 2", len(models))
	}
}

func TestRegistry_Update(t *testing.T) {
	cfg1 := config.ProviderConfig{
		Name: "p1", Type: "openai", BaseURL: "http://x",
		APIKey: "k", Timeout: time.Second,
		Models: map[string]string{"old-model": "old-real"},
	}
	p1, _ := NewProvider(cfg1)
	reg := NewRegistry([]Provider{p1})

	// Verify old model exists
	_, _, err := reg.Resolve("old-model")
	if err != nil {
		t.Fatal(err)
	}

	// Update with new provider
	cfg2 := config.ProviderConfig{
		Name: "p2", Type: "openai", BaseURL: "http://y",
		APIKey: "k", Timeout: time.Second,
		Models: map[string]string{"new-model": "new-real"},
	}
	p2, _ := NewProvider(cfg2)
	reg.Update([]Provider{p2})

	// Old model should be gone
	_, _, err = reg.Resolve("old-model")
	if err == nil {
		t.Fatal("old model should not exist after update")
	}

	// New model should exist
	provider, realName, err := reg.Resolve("new-model")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "p2" || realName != "new-real" {
		t.Errorf("unexpected: provider=%q, realName=%q", provider.Name(), realName)
	}
}

func TestBuildProviders_UnsupportedType(t *testing.T) {
	cfgs := []config.ProviderConfig{
		{Name: "bad", Type: "anthropic", BaseURL: "http://x", APIKey: "k", Models: map[string]string{"a": "b"}},
	}
	_, err := BuildProviders(cfgs)
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("error message: %v", err)
	}
}

func TestIsTimeoutError(t *testing.T) {
	if IsTimeoutError(nil) {
		t.Error("nil should not be timeout")
	}
	if !IsTimeoutError(context.DeadlineExceeded) {
		t.Error("DeadlineExceeded should be timeout")
	}
}
