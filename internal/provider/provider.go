package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/abiter/wing-ai-proxy/internal/config"
)

// ModelInfo represents a model exposed to clients.
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// Provider is the abstraction for upstream LLM backends.
type Provider interface {
	Name() string
	Type() string
	// Models returns the show_name → real_name mapping for this provider.
	Models() map[string]string
	// ChatCompletion forwards a chat request to the upstream and returns the raw HTTP response.
	// The caller is responsible for closing resp.Body.
	ChatCompletion(ctx context.Context, body []byte, clientHeaders http.Header) (*http.Response, error)
	ListModels(ctx context.Context) []ModelInfo
}

// --- OpenAI Provider ---

type openAIProvider struct {
	name    string
	baseURL string
	apiKey  string
	models  map[string]string // show_name → real_name
	client  *http.Client
}

func newOpenAIProvider(cfg config.ProviderConfig) *openAIProvider {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		MaxConnsPerHost:     500,
		IdleConnTimeout:     90 * time.Second,
	}

	return &openAIProvider{
		name:    cfg.Name,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		models:  cfg.Models,
		client: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: transport,
		},
	}
}

func (p *openAIProvider) Name() string              { return p.name }
func (p *openAIProvider) Type() string              { return "openai" }
func (p *openAIProvider) Models() map[string]string { return p.models }

func (p *openAIProvider) ChatCompletion(ctx context.Context, body []byte, clientHeaders http.Header) (*http.Response, error) {
	// Replace model field in request body
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return nil, fmt.Errorf("provider: unmarshal request body: %w", err)
	}

	if showName, ok := reqBody["model"].(string); ok {
		if realName, exists := p.models[showName]; exists {
			reqBody["model"] = realName
		}
	}

	modifiedBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("provider: marshal modified body: %w", err)
	}

	url := p.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(modifiedBody))
	if err != nil {
		return nil, fmt.Errorf("provider: create request: %w", err)
	}

	// Forward client headers, except Accept-Encoding (let Go's transport handle compression transparently)
	for key, vals := range clientHeaders {
		if strings.EqualFold(key, "Accept-Encoding") {
			continue
		}
		for _, v := range vals {
			req.Header.Add(key, v)
		}
	}

	// Override authorization with provider's API key
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider: upstream request: %w", err)
	}

	return resp, nil
}

func (p *openAIProvider) ListModels(_ context.Context) []ModelInfo {
	models := make([]ModelInfo, 0, len(p.models))
	for showName := range p.models {
		models = append(models, ModelInfo{
			ID:      showName,
			Object:  "model",
			OwnedBy: p.name,
		})
	}
	return models
}

// --- Provider Registry ---

// Registry manages the mapping from show_name → Provider and resolves routing.
type Registry struct {
	mu        sync.RWMutex
	providers []Provider
	modelMap  map[string]providerEntry // show_name → (provider, real_name)
}

type providerEntry struct {
	provider Provider
	realName string
}

// NewRegistry creates a registry from a list of providers.
// It logs warnings for conflicting show_names.
func NewRegistry(providers []Provider) *Registry {
	r := &Registry{
		providers: providers,
		modelMap:  make(map[string]providerEntry),
	}

	for _, p := range providers {
		for showName, realName := range p.Models() {
			if existing, ok := r.modelMap[showName]; ok {
				slog.Warn("model show_name conflict",
					"show_name", showName,
					"existing_provider", existing.provider.Name(),
					"conflicting_provider", p.Name(),
					"resolution", "using first provider",
				)
				continue
			}
			r.modelMap[showName] = providerEntry{provider: p, realName: realName}
		}
	}

	return r
}

// Resolve looks up the provider and real model name for a given show_name.
func (r *Registry) Resolve(showName string) (Provider, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.modelMap[showName]
	if !ok {
		return nil, "", fmt.Errorf("model %q not found", showName)
	}
	return entry.provider, entry.realName, nil
}

// AllModels returns all show_names with their provider info.
func (r *Registry) AllModels() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var models []ModelInfo
	for showName, entry := range r.modelMap {
		models = append(models, ModelInfo{
			ID:      showName,
			Object:  "model",
			OwnedBy: entry.provider.Name(),
		})
	}
	return models
}

// ShowNames returns all configured show names.
func (r *Registry) ShowNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.modelMap))
	for name := range r.modelMap {
		names = append(names, name)
	}
	return names
}

// Update replaces the registry with new providers, rebuilding the model map.
func (r *Registry) Update(providers []Provider) {
	newReg := NewRegistry(providers)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = newReg.providers
	r.modelMap = newReg.modelMap
}

// NewProvider creates a Provider from configuration.
func NewProvider(cfg config.ProviderConfig) (Provider, error) {
	switch cfg.Type {
	case "openai":
		return newOpenAIProvider(cfg), nil
	default:
		return nil, fmt.Errorf("provider: unsupported type %q", cfg.Type)
	}
}

// BuildProviders creates all providers from config, logging errors for failures.
func BuildProviders(cfgs []config.ProviderConfig) ([]Provider, error) {
	var providers []Provider
	for _, cfg := range cfgs {
		p, err := NewProvider(cfg)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", cfg.Name, err)
		}
		providers = append(providers, p)
	}
	return providers, nil
}

// IsTimeoutError checks if the error is a timeout.
func IsTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	// Check for context deadline exceeded (possibly wrapped)
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Check net.Error timeout interface (possibly wrapped)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Fallback: check error string for timeout indicators
	errStr := err.Error()
	return strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded")
}

// ReadBody reads and closes a response body.
func ReadBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
