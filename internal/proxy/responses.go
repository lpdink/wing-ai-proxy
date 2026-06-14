package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/abiter/wing-ai-proxy/internal/audit"
	"github.com/abiter/wing-ai-proxy/internal/middleware"
)

// Responses handles POST /v1/responses — bridges OpenAI Responses API to upstream Chat Completions.
func (h *Handler) Responses(c *gin.Context) {
	logger := middleware.GetLogger(c)
	requestID := middleware.GetRequestID(c)
	startTime := time.Now()

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.Error("failed to read request body", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	// Parse Responses API request
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	// Resolve provider and real model name
	p, realName, err := h.registry.Resolve(req.Model)
	if err != nil {
		availableModels := h.registry.ShowNames()
		c.JSON(http.StatusBadRequest, gin.H{
			"error":            fmt.Sprintf("model %q not found", req.Model),
			"available_models": availableModels,
		})
		return
	}

	logger = logger.With("provider", p.Name(), "model_show", req.Model, "model_real", realName, "api", "responses")

	virtualKey, _ := c.Get(middleware.CtxAPIKey)
	auditRecord := audit.Record{
		RequestID:     requestID,
		VirtualAPIKey: virtualKey.(string),
		ProviderName:  p.Name(),
		ModelShowName: req.Model,
		ModelRealName: realName,
		RequestStart:  startTime,
		IsStream:      req.Stream,
		RequestBody:   string(body),
	}

	// Convert Responses API request to Chat Completions format
	chatBody := convertToChatCompletions(&req, realName)
	chatBytes, err := json.Marshal(chatBody)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to convert request"})
		return
	}

	// Forward to upstream
	ctx := c.Request.Context()
	resp, err := p.ChatCompletion(ctx, chatBytes, c.Request.Header)
	if err != nil {
		h.handleUpstreamError(c, err, &auditRecord, logger, startTime)
		return
	}
	defer resp.Body.Close()

	auditRecord.StatusCode = resp.StatusCode
	auditRecord.FirstByteAt = time.Now()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		auditRecord.ResponseBody = string(respBody)
		auditRecord.RequestEnd = time.Now()
		h.recordMetrics(&auditRecord, startTime)
		h.auditWriter.Submit(auditRecord)
		// 🔴 Fix 4: forward upstream Content-Type
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			c.Header("Content-Type", ct)
		}
		c.Status(resp.StatusCode)
		c.Writer.Write(respBody)
		return
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusOK)

	flusher, _ := c.Writer.(interface{ Flush() })

	// Stream-translate Chat Completions SSE → Responses API SSE
	responseID := fmt.Sprintf("resp_%s", requestID)
	translator := &responsesTranslator{
		responseID: responseID,
		model:      req.Model,
		writer:     c.Writer,
		flusher:    flusher,
		logger:     logger,
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var aggregatedContent strings.Builder
	var aggregatedReasoning strings.Builder
	var toolCalls []translatedToolCall
	var usageInfo *chatUsage

	// Emit response.created
	translator.emitCreated()

	// Emit output_item.added for the message
	translator.emitMessageItemAdded()
	// Emit content_part.added for the text content
	translator.emitContentPartAdded()

	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines and non-data lines
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		// Parse SSE data
		var chunk chatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			logger.Warn("failed to parse SSE chunk", "error", err, "data", data)
			continue
		}

		// Extract usage if present
		if chunk.Usage != nil {
			usageInfo = chunk.Usage
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta

		// Handle content text
		if delta.Content != "" {
			aggregatedContent.WriteString(delta.Content)
			translator.emitTextDelta(delta.Content)
		}

		// Handle reasoning content (some models use reasoning_content)
		if delta.ReasoningContent != "" {
			aggregatedReasoning.WriteString(delta.ReasoningContent)
		}

		// Handle tool calls
		for _, tc := range delta.ToolCalls {
			idx := tc.Index
			for len(toolCalls) <= idx {
				toolCalls = append(toolCalls, translatedToolCall{})
			}
			if tc.Function.Name != "" {
				toolCalls[idx].name = tc.Function.Name
				toolCalls[idx].callID = tc.ID
				translator.emitToolCallAdded(idx, tc.ID, tc.Function.Name)
			}
			if tc.Function.Arguments != "" {
				toolCalls[idx].args.WriteString(tc.Function.Arguments)
				translator.emitToolCallArgsDelta(idx, toolCalls[idx].callID, tc.Function.Arguments)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Error("stream scan error", "error", err)
		auditRecord.ErrorMessage = fmt.Sprintf("stream: %v", err)
	}

	// Emit response.completed
	translator.emitCompleted(aggregatedContent.String(), aggregatedReasoning.String(), toolCalls, usageInfo)

	auditRecord.RequestEnd = time.Now()
	auditRecord.ResponseBody = aggregatedContent.String()
	if usageInfo != nil {
		auditRecord.InputTokens = usageInfo.PromptTokens
		auditRecord.OutputTokens = usageInfo.CompletionTokens
		auditRecord.CacheHitTokens = usageInfo.PromptTokensDetails.CachedTokens
	}

	h.recordMetrics(&auditRecord, startTime)
	h.auditWriter.Submit(auditRecord)

	logger.Info("responses stream completed",
		"duration_ms", auditRecord.RequestEnd.Sub(auditRecord.RequestStart).Milliseconds(),
		"input_tokens", auditRecord.InputTokens,
		"output_tokens", auditRecord.OutputTokens,
	)
}

// --- Responses API types ---

type ResponsesRequest struct {
	Model        string            `json:"model"`
	Instructions string            `json:"instructions"`
	Input        []ResponseItem    `json:"input"`
	Tools        []json.RawMessage `json:"tools"`
	ToolChoice   string            `json:"tool_choice"`
	Stream       bool              `json:"stream"`
	Reasoning    *json.RawMessage  `json:"reasoning,omitempty"`
}

type ResponseItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// For function_call_output
	CallID string `json:"call_id,omitempty"`
	Output string `json:"output,omitempty"`
	// For function_call
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	ID        string `json:"id,omitempty"`
}

// --- Chat Completions types ---

type chatCompletionRequest struct {
	Model         string            `json:"model"`
	Messages      []chatMessage     `json:"messages"`
	Tools         []json.RawMessage `json:"tools,omitempty"`
	Stream        bool              `json:"stream"`
	StreamOptions *streamOptions    `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []toolCallMsg   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type toolCallMsg struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function functionCallMsg `json:"function"`
}

type functionCallMsg struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatCompletionChunk struct {
	ID      string        `json:"id"`
	Choices []chunkChoice `json:"choices"`
	Usage   *chatUsage    `json:"usage,omitempty"`
}

type chunkChoice struct {
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCallDelta `json:"tool_calls,omitempty"`
}

type toolCallDelta struct {
	Index    int           `json:"index"`
	ID       string        `json:"id,omitempty"`
	Type     string        `json:"type,omitempty"`
	Function functionDelta `json:"function,omitempty"`
}

type functionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type chatUsage struct {
	PromptTokens        int                 `json:"prompt_tokens"`
	CompletionTokens    int                 `json:"completion_tokens"`
	TotalTokens         int                 `json:"total_tokens"`
	PromptTokensDetails promptTokensDetails `json:"prompt_tokens_details"`
}

type promptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// --- Conversion ---

func convertToChatCompletions(req *ResponsesRequest, realModel string) chatCompletionRequest {
	var messages []chatMessage

	// Add instructions as system message
	if req.Instructions != "" {
		content, _ := json.Marshal(req.Instructions)
		messages = append(messages, chatMessage{
			Role:    "system",
			Content: content,
		})
	}

	// Convert input items to messages
	for _, item := range req.Input {
		switch item.Type {
		case "message":
			role := item.Role
			// Responses API uses "developer" for system-level instructions;
			// upstream Chat Completions providers expect "system".
			if role == "developer" {
				role = "system"
			}
			msg := chatMessage{
				Role:    role,
				Content: normalizeContent(item.Content),
			}
			messages = append(messages, msg)

		case "function_call":
			// Convert to assistant message with tool_calls
			content, _ := json.Marshal("")
			messages = append(messages, chatMessage{
				Role:    "assistant",
				Content: content,
				ToolCalls: []toolCallMsg{{
					ID:   item.ID,
					Type: "function",
					Function: functionCallMsg{
						Name:      item.Name,
						Arguments: item.Arguments,
					},
				}},
			})

		case "function_call_output":
			content, _ := json.Marshal(item.Output)
			messages = append(messages, chatMessage{
				Role:       "tool",
				Content:    content,
				ToolCallID: item.CallID,
			})
		}
	}

	chatReq := chatCompletionRequest{
		Model:    realModel,
		Messages: messages,
		Stream:   true,
		StreamOptions: &streamOptions{
			IncludeUsage: true,
		},
	}

	// Convert tools - Responses API tools are mostly compatible with Chat Completions
	if len(req.Tools) > 0 {
		chatReq.Tools = convertTools(req.Tools)
	}

	return chatReq
}

// normalizeContent ensures content is valid JSON for Chat Completions format.
// Responses API content can be a string, array of content parts, or null.
func normalizeContent(raw json.RawMessage) json.RawMessage {
	if raw == nil || string(raw) == "null" {
		b, _ := json.Marshal("")
		return b
	}

	// Try as string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return raw // already a string
	}

	// Try as array of content parts → convert to string concatenation
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "output_text" || p.Type == "text" || p.Type == "input_text" {
				sb.WriteString(p.Text)
			} else {
				slog.Warn("normalizeContent: dropping non-text content part", "type", p.Type)
			}
		}
		b, _ := json.Marshal(sb.String())
		return b
	}

	return raw
}

// convertTools adapts Responses API tool definitions to Chat Completions format.
// Responses API: {"type":"function","name":"bash","description":"...","parameters":{...}}
// Chat Completions: {"type":"function","function":{"name":"bash","description":"...","parameters":{...}}}
func convertTools(tools []json.RawMessage) []json.RawMessage {
	result := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		var tool map[string]json.RawMessage
		if err := json.Unmarshal(t, &tool); err != nil {
			continue
		}

		toolType, _ := tool["type"]
		var typeStr string
		json.Unmarshal(toolType, &typeStr)

		if typeStr == "function" {
			// Check if already in Chat Completions format (has "function" key)
			if _, hasFunction := tool["function"]; hasFunction {
				result = append(result, t)
				continue
			}

			// Wrap Responses API format into Chat Completions format
			fnDef := make(map[string]json.RawMessage)
			for _, key := range []string{"name", "description", "parameters", "strict"} {
				if v, ok := tool[key]; ok {
					fnDef[key] = v
				}
			}
			fnBytes, err := json.Marshal(fnDef)
			if err != nil {
				continue
			}

			wrapped := map[string]json.RawMessage{
				"type":     toolType,
				"function": fnBytes,
			}
			wrappedBytes, err := json.Marshal(wrapped)
			if err != nil {
				continue
			}
			result = append(result, wrappedBytes)
		} else {
			slog.Warn("convertTools: dropping non-function tool", "type", typeStr)
		}
	}
	return result
}

// --- SSE Translator ---

type translatedToolCall struct {
	name   string
	callID string
	args   strings.Builder
}

type responsesTranslator struct {
	responseID string
	model      string
	writer     io.Writer
	flusher    interface{ Flush() }
	logger     *slog.Logger
	itemIndex  int
}

func (t *responsesTranslator) writeEvent(eventType string, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		t.logger.Warn("failed to marshal SSE event", "error", err)
		return
	}
	fmt.Fprintf(t.writer, "event: %s\ndata: %s\n\n", eventType, jsonData)
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

func (t *responsesTranslator) emitCreated() {
	t.writeEvent("response.created", map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id":     t.responseID,
			"object": "response",
			"model":  t.model,
			"status": "in_progress",
		},
	})
}

func (t *responsesTranslator) emitTextDelta(delta string) {
	t.writeEvent("response.output_text.delta", map[string]interface{}{
		"type":          "response.output_text.delta",
		"item_id":       fmt.Sprintf("msg_%d", t.itemIndex),
		"output_index":  t.itemIndex,
		"content_index": 0,
		"delta":         delta,
	})
}

func (t *responsesTranslator) emitToolCallAdded(idx int, callID, name string) {
	t.writeEvent("response.output_item.added", map[string]interface{}{
		"type":         "response.output_item.added",
		"output_index": t.itemIndex + 1 + idx,
		"item": map[string]interface{}{
			"type":      "function_call",
			"id":        callID,
			"call_id":   callID,
			"name":      name,
			"arguments": "",
			"status":    "in_progress",
		},
	})
}

func (t *responsesTranslator) emitToolCallArgsDelta(idx int, callID, delta string) {
	t.writeEvent("response.function_call_arguments.delta", map[string]interface{}{
		"type":         "response.function_call_arguments.delta",
		"item_id":      fmt.Sprintf("fc_%d", idx),
		"output_index": t.itemIndex + 1 + idx,
		"call_id":      callID,
		"delta":        delta,
	})
}

func (t *responsesTranslator) emitMessageItemAdded() {
	t.writeEvent("response.output_item.added", map[string]interface{}{
		"type":         "response.output_item.added",
		"output_index": t.itemIndex,
		"item": map[string]interface{}{
			"type":    "message",
			"id":      fmt.Sprintf("msg_%d", t.itemIndex),
			"role":    "assistant",
			"content": []interface{}{},
			"status":  "in_progress",
		},
	})
}

func (t *responsesTranslator) emitContentPartAdded() {
	t.writeEvent("response.content_part.added", map[string]interface{}{
		"type":          "response.content_part.added",
		"item_id":       fmt.Sprintf("msg_%d", t.itemIndex),
		"output_index":  t.itemIndex,
		"content_index": 0,
		"part": map[string]interface{}{
			"type": "output_text",
			"text": "",
		},
	})
}

func (t *responsesTranslator) emitTextDone(content string) {
	t.writeEvent("response.output_text.done", map[string]interface{}{
		"type":          "response.output_text.done",
		"item_id":       fmt.Sprintf("msg_%d", t.itemIndex),
		"output_index":  t.itemIndex,
		"content_index": 0,
		"text":          content,
	})
}

func (t *responsesTranslator) emitMessageItemDone(content string) {
	t.writeEvent("response.output_item.done", map[string]interface{}{
		"type":         "response.output_item.done",
		"output_index": t.itemIndex,
		"item": map[string]interface{}{
			"type": "message",
			"id":   fmt.Sprintf("msg_%d", t.itemIndex),
			"role": "assistant",
			"content": []map[string]interface{}{
				{"type": "output_text", "text": content},
			},
			"status": "completed",
		},
	})
}

func (t *responsesTranslator) emitToolCallArgsDone(idx int, tc translatedToolCall) {
	t.writeEvent("response.function_call_arguments.done", map[string]interface{}{
		"type":         "response.function_call_arguments.done",
		"item_id":      fmt.Sprintf("fc_%d", idx),
		"output_index": t.itemIndex + 1 + idx,
		"call_id":      tc.callID,
		"arguments":    tc.args.String(),
	})
}

func (t *responsesTranslator) emitToolCallItemDone(idx int, tc translatedToolCall) {
	t.writeEvent("response.output_item.done", map[string]interface{}{
		"type":         "response.output_item.done",
		"output_index": t.itemIndex + 1 + idx,
		"item": map[string]interface{}{
			"type":      "function_call",
			"id":        fmt.Sprintf("fc_%d", idx),
			"call_id":   tc.callID,
			"name":      tc.name,
			"arguments": tc.args.String(),
			"status":    "completed",
		},
	})
}

func (t *responsesTranslator) emitCompleted(content, reasoning string, toolCalls []translatedToolCall, usage *chatUsage) {
	// Emit text done and item done if there was content
	if content != "" || reasoning != "" {
		t.emitTextDone(content)
		t.emitMessageItemDone(content)
	}

	// Emit tool call done events
	for i, tc := range toolCalls {
		t.emitToolCallArgsDone(i, tc)
		t.emitToolCallItemDone(i, tc)
	}

	// Build output items for the final completed event
	var output []interface{}

	// Add message item if there's content
	if content != "" || reasoning != "" {
		contentParts := []map[string]interface{}{
			{
				"type": "output_text",
				"text": content,
			},
		}
		item := map[string]interface{}{
			"type":    "message",
			"id":      fmt.Sprintf("msg_%d", t.itemIndex),
			"role":    "assistant",
			"content": contentParts,
			"status":  "completed",
		}
		output = append(output, item)
	}

	// Add function call items
	for i, tc := range toolCalls {
		item := map[string]interface{}{
			"type":      "function_call",
			"id":        fmt.Sprintf("fc_%d", i),
			"call_id":   tc.callID,
			"name":      tc.name,
			"arguments": tc.args.String(),
			"status":    "completed",
		}
		output = append(output, item)
	}

	// Build usage
	usageObj := map[string]interface{}{
		"input_tokens":  0,
		"output_tokens": 0,
		"total_tokens":  0,
	}
	if usage != nil {
		usageObj["input_tokens"] = usage.PromptTokens
		usageObj["output_tokens"] = usage.CompletionTokens
		usageObj["total_tokens"] = usage.TotalTokens
		usageObj["input_tokens_details"] = map[string]interface{}{
			"cached_tokens": usage.PromptTokensDetails.CachedTokens,
		}
	}

	t.writeEvent("response.completed", map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id":     t.responseID,
			"object": "response",
			"model":  t.model,
			"status": "completed",
			"output": output,
			"usage":  usageObj,
		},
	})
}
