package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/abiter/wing-ai-proxy/internal/audit"
	"github.com/abiter/wing-ai-proxy/internal/config"
	"github.com/abiter/wing-ai-proxy/internal/middleware"
	"github.com/abiter/wing-ai-proxy/internal/provider"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupTestRouter(upstream *httptest.Server, models map[string]string) (*gin.Engine, *audit.AsyncWriter) {
	cfg := config.ProviderConfig{
		Name:    "test-provider",
		Type:    "openai",
		BaseURL: upstream.URL,
		APIKey:  "sk-test",
		Timeout: 30 * time.Second,
		Models:  models,
	}

	p, _ := provider.NewProvider(cfg)
	reg := provider.NewRegistry([]provider.Provider{p})

	// Create in-memory audit store (use temp file)
	store, _ := audit.NewSQLiteStore(":memory:")
	writer := audit.NewAsyncWriter(store, 100)

	handler := NewHandler(reg, writer)
	auth := middleware.NewAuth([]string{"sk-valid"})

	r := gin.New()
	r.Use(middleware.RequestID())

	// Public routes
	r.GET("/health", Health)

	// Protected routes
	api := r.Group("/")
	api.Use(auth.Handler())
	api.GET("/models", handler.Models)
	api.POST("/chat/completions", handler.ChatCompletions)

	return r, writer
}

func TestModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	r, writer := setupTestRouter(upstream, map[string]string{"gpt-4": "gpt-4-turbo", "gpt-3.5": "gpt-3.5-turbo"})
	defer writer.Drain()

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/models", nil)
	req.Header.Set("Authorization", "Bearer sk-valid")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Object != "list" {
		t.Errorf("object = %q, want %q", resp.Object, "list")
	}
	if len(resp.Data) != 2 {
		t.Errorf("models count = %d, want 2", len(resp.Data))
	}
}

func TestChatCompletions_NonStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)

		// Verify model was replaced
		if req["model"] != "gpt-4-turbo" {
			t.Errorf("model not replaced: got %v", req["model"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"Hello!"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0}}}`))
	}))
	defer upstream.Close()

	r, writer := setupTestRouter(upstream, map[string]string{"gpt-4": "gpt-4-turbo"})
	defer writer.Drain()

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-valid")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "chatcmpl-1" {
		t.Errorf("response id = %v", resp["id"])
	}
}

func TestChatCompletions_Stream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n",
			`data: {"choices":[{"index":0,"delta":{"content":"Hello"}}]}` + "\n\n",
			`data: {"choices":[{"index":0,"delta":{"content":" world"}}]}` + "\n\n",
			`data: {"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":0}}}` + "\n\n",
			`data: [DONE]` + "\n\n",
		}

		for _, chunk := range chunks {
			fmt.Fprint(w, chunk)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	r, writer := setupTestRouter(upstream, map[string]string{"gpt-4": "gpt-4-turbo"})
	defer writer.Drain()

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-valid")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Hello") {
		t.Error("stream response missing content")
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("stream response missing [DONE]")
	}
}

func TestChatCompletions_ModelNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer upstream.Close()

	r, writer := setupTestRouter(upstream, map[string]string{"gpt-4": "gpt-4-turbo"})
	defer writer.Drain()

	reqBody := `{"model":"unknown-model","messages":[{"role":"user","content":"Hi"}]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-valid")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["available_models"] == nil {
		t.Error("response should include available_models")
	}
}

func TestChatCompletions_UpstreamTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	// Create provider with very short timeout
	cfg := config.ProviderConfig{
		Name:    "slow",
		Type:    "openai",
		BaseURL: upstream.URL,
		APIKey:  "k",
		Timeout: 100 * time.Millisecond,
		Models:  map[string]string{"m": "m"},
	}
	p, _ := provider.NewProvider(cfg)
	reg := provider.NewRegistry([]provider.Provider{p})

	store, _ := audit.NewSQLiteStore(":memory:")
	writer := audit.NewAsyncWriter(store, 100)
	handler := NewHandler(reg, writer)
	auth := middleware.NewAuth([]string{"sk-valid"})

	r := gin.New()
	r.Use(middleware.RequestID())
	api := r.Group("/")
	api.Use(auth.Handler())
	api.POST("/chat/completions", handler.ChatCompletions)

	defer writer.Drain()

	reqBody := `{"model":"m","messages":[{"role":"user","content":"Hi"}]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-valid")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 504 {
		t.Errorf("status = %d, want 504", w.Code)
	}
}

func TestChatCompletions_UpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
	}))
	defer upstream.Close()

	r, writer := setupTestRouter(upstream, map[string]string{"gpt-4": "gpt-4-turbo"})
	defer writer.Drain()

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-valid")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// 4xx should be transparently forwarded
	if w.Code != 429 {
		t.Errorf("status = %d, want 429", w.Code)
	}
}

func TestHealth(t *testing.T) {
	r := gin.New()
	r.GET("/health", Health)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
}

func TestCustomHeaderForwarding(t *testing.T) {
	var receivedHeaders http.Header

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"1"}`))
	}))
	defer upstream.Close()

	r, writer := setupTestRouter(upstream, map[string]string{"m": "m"})
	defer writer.Drain()

	reqBody := `{"model":"m","messages":[]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-valid")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom-Header", "test-value")
	req.Header.Set("X-Trace-Id", "abc-123")
	r.ServeHTTP(w, req)

	if receivedHeaders.Get("X-Custom-Header") != "test-value" {
		t.Errorf("custom header not forwarded")
	}
	if receivedHeaders.Get("X-Trace-Id") != "abc-123" {
		t.Errorf("trace header not forwarded")
	}
}
