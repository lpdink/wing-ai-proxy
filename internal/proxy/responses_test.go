package proxy

import (
	"bytes"
	"encoding/json"
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

// --- convertToChatCompletions ---

func TestConvertToChatCompletions_BasicMessage(t *testing.T) {
	req := &ResponsesRequest{
		Model: "qwen3.7-max",
		Input: []ResponseItem{
			{Type: "message", Role: "user", Content: jsonRaw(`"hello"`)},
		},
		Stream: true,
	}

	chat := convertToChatCompletions(req, "qwen3.7-max")

	if chat.Model != "qwen3.7-max" {
		t.Errorf("model = %q, want %q", chat.Model, "qwen3.7-max")
	}
	if !chat.Stream {
		t.Error("stream should be true")
	}
	if chat.StreamOptions == nil || !chat.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage should be true")
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(chat.Messages))
	}
	if chat.Messages[0].Role != "user" {
		t.Errorf("messages[0].role = %q, want %q", chat.Messages[0].Role, "user")
	}
}

func TestConvertToChatCompletions_InstructionsBecomeSystem(t *testing.T) {
	req := &ResponsesRequest{
		Model:        "test-model",
		Instructions: "You are a helpful assistant.",
		Input: []ResponseItem{
			{Type: "message", Role: "user", Content: jsonRaw(`"hi"`)},
		},
	}

	chat := convertToChatCompletions(req, "test-model")

	if len(chat.Messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(chat.Messages))
	}
	if chat.Messages[0].Role != "system" {
		t.Errorf("messages[0].role = %q, want %q", chat.Messages[0].Role, "system")
	}
	var sysContent string
	json.Unmarshal(chat.Messages[0].Content, &sysContent)
	if sysContent != "You are a helpful assistant." {
		t.Errorf("system content = %q, want %q", sysContent, "You are a helpful assistant.")
	}
}

func TestConvertToChatCompletions_DeveloperRoleBecomesSystem(t *testing.T) {
	req := &ResponsesRequest{
		Model: "test-model",
		Input: []ResponseItem{
			{Type: "message", Role: "developer", Content: jsonRaw(`"be concise"`)},
			{Type: "message", Role: "user", Content: jsonRaw(`"hi"`)},
		},
	}

	chat := convertToChatCompletions(req, "test-model")

	if len(chat.Messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(chat.Messages))
	}
	if chat.Messages[0].Role != "system" {
		t.Errorf("messages[0].role = %q, want %q", chat.Messages[0].Role, "system")
	}
	if chat.Messages[1].Role != "user" {
		t.Errorf("messages[1].role = %q, want %q", chat.Messages[1].Role, "user")
	}
}

func TestConvertToChatCompletions_FunctionCallAndOutput(t *testing.T) {
	req := &ResponsesRequest{
		Model: "test-model",
		Input: []ResponseItem{
			{Type: "message", Role: "user", Content: jsonRaw(`"run ls"`)},
			{
				Type:      "function_call",
				ID:        "call_1",
				Name:      "bash",
				Arguments: `{"command":"ls"}`,
			},
			{
				Type:   "function_call_output",
				CallID: "call_1",
				Output: "file1\nfile2",
			},
		},
	}

	chat := convertToChatCompletions(req, "test-model")

	if len(chat.Messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(chat.Messages))
	}

	// function_call → assistant with tool_calls
	assistantMsg := chat.Messages[1]
	if assistantMsg.Role != "assistant" {
		t.Errorf("messages[1].role = %q, want %q", assistantMsg.Role, "assistant")
	}
	if len(assistantMsg.ToolCalls) != 1 {
		t.Fatalf("len(tool_calls) = %d, want 1", len(assistantMsg.ToolCalls))
	}
	if assistantMsg.ToolCalls[0].ID != "call_1" {
		t.Errorf("tool_call.id = %q, want %q", assistantMsg.ToolCalls[0].ID, "call_1")
	}
	if assistantMsg.ToolCalls[0].Function.Name != "bash" {
		t.Errorf("tool_call.function.name = %q, want %q", assistantMsg.ToolCalls[0].Function.Name, "bash")
	}

	// function_call_output → tool role
	toolMsg := chat.Messages[2]
	if toolMsg.Role != "tool" {
		t.Errorf("messages[2].role = %q, want %q", toolMsg.Role, "tool")
	}
	if toolMsg.ToolCallID != "call_1" {
		t.Errorf("tool_call_id = %q, want %q", toolMsg.ToolCallID, "call_1")
	}
}

// --- normalizeContent ---

func TestNormalizeContent_String(t *testing.T) {
	raw := jsonRaw(`"hello world"`)
	result := normalizeContent(raw)
	var s string
	json.Unmarshal(result, &s)
	if s != "hello world" {
		t.Errorf("got %q, want %q", s, "hello world")
	}
}

func TestNormalizeContent_Null(t *testing.T) {
	result := normalizeContent(nil)
	var s string
	json.Unmarshal(result, &s)
	if s != "" {
		t.Errorf("got %q, want empty", s)
	}
}

func TestNormalizeContent_ArrayOfParts(t *testing.T) {
	raw := jsonRaw(`[{"type":"output_text","text":"hello "},{"type":"output_text","text":"world"}]`)
	result := normalizeContent(raw)
	var s string
	json.Unmarshal(result, &s)
	if s != "hello world" {
		t.Errorf("got %q, want %q", s, "hello world")
	}
}

// --- convertTools ---

func TestConvertTools_WrapsResponsesFormat(t *testing.T) {
	tools := []json.RawMessage{
		jsonRaw(`{"type":"function","name":"bash","description":"Run shell","parameters":{"type":"object"}}`),
	}

	result := convertTools(tools)
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	var wrapped map[string]json.RawMessage
	json.Unmarshal(result[0], &wrapped)

	if _, ok := wrapped["function"]; !ok {
		t.Fatal("missing 'function' key in wrapped tool")
	}

	var fnDef map[string]json.RawMessage
	json.Unmarshal(wrapped["function"], &fnDef)

	var name string
	json.Unmarshal(fnDef["name"], &name)
	if name != "bash" {
		t.Errorf("function.name = %q, want %q", name, "bash")
	}
}

func TestConvertTools_PassesThroughChatFormat(t *testing.T) {
	tools := []json.RawMessage{
		jsonRaw(`{"type":"function","function":{"name":"bash","description":"Run shell","parameters":{}}}`),
	}

	result := convertTools(tools)
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	// Should be unchanged
	var tool map[string]json.RawMessage
	json.Unmarshal(result[0], &tool)
	if _, ok := tool["function"]; !ok {
		t.Fatal("missing 'function' key")
	}
}

// --- responsesTranslator SSE output ---

func TestTranslator_EmitCreated(t *testing.T) {
	var buf bytes.Buffer
	tr := &responsesTranslator{
		responseID: "resp_123",
		model:      "test-model",
		writer:     &buf,
	}

	tr.emitCreated()

	output := buf.String()
	if !strings.Contains(output, "event: response.created") {
		t.Error("missing response.created event")
	}
	if !strings.Contains(output, "resp_123") {
		t.Error("missing response ID")
	}
	if !strings.Contains(output, "in_progress") {
		t.Error("missing in_progress status")
	}
}

func TestTranslator_EmitTextDelta(t *testing.T) {
	var buf bytes.Buffer
	tr := &responsesTranslator{
		responseID: "resp_123",
		writer:     &buf,
	}

	tr.emitTextDelta("hello")

	output := buf.String()
	if !strings.Contains(output, "event: response.output_text.delta") {
		t.Error("missing response.output_text.delta event")
	}
	if !strings.Contains(output, `"delta":"hello"`) {
		t.Error("missing delta content")
	}
}

func TestTranslator_FullLifecycle(t *testing.T) {
	var buf bytes.Buffer
	tr := &responsesTranslator{
		responseID: "resp_123",
		model:      "test-model",
		writer:     &buf,
	}

	// Full lifecycle
	tr.emitCreated()
	tr.emitMessageItemAdded()
	tr.emitContentPartAdded()
	tr.emitTextDelta("Hi")
	tr.emitTextDelta(" there")
	tr.emitTextDone("Hi there")
	tr.emitMessageItemDone()
	tr.emitCompleted("Hi there", "", nil, &chatUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
	})

	output := buf.String()

	// Check all event types present
	events := []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	for _, ev := range events {
		if !strings.Contains(output, "event: "+ev) {
			t.Errorf("missing event: %s", ev)
		}
	}

	// Check completed has usage
	if !strings.Contains(output, `"input_tokens":10`) {
		t.Error("missing input_tokens in completed event")
	}
	if !strings.Contains(output, `"output_tokens":5`) {
		t.Error("missing output_tokens in completed event")
	}
}

func TestTranslator_ToolCallLifecycle(t *testing.T) {
	var buf bytes.Buffer
	tr := &responsesTranslator{
		responseID: "resp_123",
		model:      "test-model",
		writer:     &buf,
	}

	tr.emitToolCallAdded(0, "call_abc", "bash")
	tr.emitToolCallArgsDelta(0, `{"command":"ls"}`)

	var tc translatedToolCall
	tc.name = "bash"
	tc.callID = "call_abc"
	tc.args.WriteString(`{"command":"ls"}`)

	tr.emitToolCallArgsDone(0, `{"command":"ls"}`)
	tr.emitToolCallItemDone(0, tc)

	output := buf.String()

	if !strings.Contains(output, "response.output_item.added") {
		t.Error("missing output_item.added for tool call")
	}
	if !strings.Contains(output, "response.function_call_arguments.delta") {
		t.Error("missing function_call_arguments.delta")
	}
	if !strings.Contains(output, "response.function_call_arguments.done") {
		t.Error("missing function_call_arguments.done")
	}
	if !strings.Contains(output, "response.output_item.done") {
		t.Error("missing output_item.done for tool call")
	}
	if !strings.Contains(output, "call_abc") {
		t.Error("missing call_id")
	}
}

// --- Integration: /v1/responses endpoint ---

func TestResponsesEndpoint(t *testing.T) {
	// Mock upstream that returns a simple streaming chat completion
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Send chunks
		chunks := []string{
			`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":" world"}}]}`,
			`data: {"id":"chatcmpl-1","choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":0}}}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			io.WriteString(w, chunk+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()

	models := map[string]string{"test-model": "real-model"}
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
	store, _ := audit.NewSQLiteStore(":memory:")
	writer := audit.NewAsyncWriter(store, 100)
	handler := NewHandler(reg, writer)
	auth := middleware.NewAuth([]string{"sk-valid"})

	r := gin.New()
	r.Use(middleware.RequestID())
	api := r.Group("/")
	api.Use(auth.Handler())
	api.POST("/v1/responses", handler.Responses)

	// Build request
	reqBody := `{"model":"test-model","input":[{"type":"message","role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-valid")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()

	// Check SSE events
	if !strings.Contains(body, "event: response.created") {
		t.Error("missing response.created")
	}
	if !strings.Contains(body, `"delta":"Hello"`) {
		t.Error("missing first text delta")
	}
	if !strings.Contains(body, `"delta":" world"`) {
		t.Error("missing second text delta")
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Error("missing response.completed")
	}
}

// --- Helpers ---

func jsonRaw(s string) json.RawMessage {
	return json.RawMessage(s)
}
