package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSQLiteStore_InsertAndQuery(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db")

	store, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Verify DB file was created
	if _, err := os.Stat(dsn); os.IsNotExist(err) {
		t.Fatal("database file not created")
	}

	record := Record{
		RequestID:      "req-001",
		VirtualAPIKey:  "sk-test",
		ProviderName:   "test-provider",
		ModelShowName:  "gpt-4",
		ModelRealName:  "gpt-4-turbo",
		RequestStart:   time.Now().Add(-2 * time.Second),
		FirstByteAt:    time.Now().Add(-1 * time.Second),
		RequestEnd:     time.Now(),
		InputTokens:    100,
		OutputTokens:   200,
		CacheHitTokens: 50,
		ToolCalls:      []string{"search", "calculator"},
		IsStream:       false,
		RequestBody:    `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`,
		ResponseBody:   `{"id":"chatcmpl-1"}`,
		StatusCode:     200,
	}

	if err := store.Insert(record); err != nil {
		t.Fatalf("insert error: %v", err)
	}

	// Query it back
	var requestID string
	err = store.db.QueryRow("SELECT request_id FROM audit_records WHERE request_id = ?", "req-001").Scan(&requestID)
	if err != nil {
		t.Fatalf("query error: %v", err)
	}
	if requestID != "req-001" {
		t.Errorf("request_id = %q, want %q", requestID, "req-001")
	}

	// Verify tool_calls stored as JSON
	var toolCallsJSON string
	err = store.db.QueryRow("SELECT tool_calls FROM audit_records WHERE request_id = ?", "req-001").Scan(&toolCallsJSON)
	if err != nil {
		t.Fatal(err)
	}
	var toolCalls []string
	json.Unmarshal([]byte(toolCallsJSON), &toolCalls)
	if len(toolCalls) != 2 || toolCalls[0] != "search" {
		t.Errorf("tool_calls = %v, want [search, calculator]", toolCalls)
	}
}

func TestSQLiteStore_DuplicateRequestID(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	r := Record{RequestID: "dup-001", RequestStart: time.Now(), FirstByteAt: time.Now(), RequestEnd: time.Now()}
	if err := store.Insert(r); err != nil {
		t.Fatal(err)
	}

	// Second insert should fail (UNIQUE constraint)
	if err := store.Insert(r); err == nil {
		t.Fatal("expected error for duplicate request_id")
	}
}

func TestAsyncWriter_BasicSubmit(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	writer := NewAsyncWriter(store, 100)

	// Submit some records
	for i := 0; i < 10; i++ {
		writer.Submit(Record{
			RequestID:    fmt.Sprintf("req-%03d", i),
			RequestStart: time.Now(),
			FirstByteAt:  time.Now(),
			RequestEnd:   time.Now(),
		})
	}

	// Drain ensures all records are processed
	writer.Drain()
	store.Close()

	// Reopen and verify count
	store2, _ := NewSQLiteStore(filepath.Join(dir, "test.db"))
	defer store2.Close()

	var count int
	store2.db.QueryRow("SELECT COUNT(*) FROM audit_records").Scan(&count)
	if count != 10 {
		t.Errorf("record count = %d, want 10", count)
	}
}

func TestAsyncWriter_QueueFull(t *testing.T) {
	// Create a slow store that blocks
	slowStore := &mockStore{delay: 100 * time.Millisecond}
	writer := NewAsyncWriter(slowStore, 2) // tiny buffer

	// Fill the queue
	for i := 0; i < 10; i++ {
		writer.Submit(Record{
			RequestID:    fmt.Sprintf("req-%03d", i),
			RequestStart: time.Now(),
			FirstByteAt:  time.Now(),
			RequestEnd:   time.Now(),
		})
	}

	writer.Drain()

	// Some records should have been dropped
	if slowStore.count >= 10 {
		t.Error("expected some records to be dropped")
	}
}

func TestAsyncWriter_NonBlocking(t *testing.T) {
	store := &mockStore{delay: 50 * time.Millisecond}
	writer := NewAsyncWriter(store, 1000)

	start := time.Now()
	for i := 0; i < 100; i++ {
		writer.Submit(Record{
			RequestID:    fmt.Sprintf("req-%03d", i),
			RequestStart: time.Now(),
			FirstByteAt:  time.Now(),
			RequestEnd:   time.Now(),
		})
	}
	elapsed := time.Since(start)

	// Submit should be nearly instant (< 10ms for 100 records)
	if elapsed > 50*time.Millisecond {
		t.Errorf("submit took %v, expected non-blocking", elapsed)
	}

	writer.Drain()
}

// --- SSE Aggregator Tests ---

func TestSSEAggregator_BasicContent(t *testing.T) {
	agg := NewSSEAggregator()

	chunks := []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"Hello"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":" world"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2}}}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}

	for _, chunk := range chunks {
		agg.ProcessChunk([]byte(chunk))
	}

	body, inputTok, outputTok, cacheHit, toolNames := agg.Result()

	if inputTok != 10 {
		t.Errorf("input_tokens = %d, want 10", inputTok)
	}
	if outputTok != 5 {
		t.Errorf("output_tokens = %d, want 5", outputTok)
	}
	if cacheHit != 2 {
		t.Errorf("cache_hit_tokens = %d, want 2", cacheHit)
	}
	if len(toolNames) != 0 {
		t.Errorf("unexpected tool calls: %v", toolNames)
	}

	var msg aggregatedResponse
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("aggregated body is not valid JSON: %v (body=%q)", err, body)
	}
	if msg.Content != "Hello world" {
		t.Errorf("content = %q, want %q", msg.Content, "Hello world")
	}
	if msg.ReasoningContent != "" {
		t.Errorf("reasoning_content = %q, want empty", msg.ReasoningContent)
	}
	if msg.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", msg.FinishReason, "stop")
	}
	if len(msg.ToolCalls) != 0 {
		t.Errorf("tool_calls = %v, want empty", msg.ToolCalls)
	}
	if msg.Usage.PromptTokens != 10 || msg.Usage.CompletionTokens != 5 || msg.Usage.CachedTokens != 2 {
		t.Errorf("usage = %+v, want {10 5 2}", msg.Usage)
	}
}

func TestSSEAggregator_ReasoningContent(t *testing.T) {
	agg := NewSSEAggregator()

	chunks := []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me think"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":" step by step."}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"The answer is 42."}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}

	for _, chunk := range chunks {
		agg.ProcessChunk([]byte(chunk))
	}

	body, _, _, _, _ := agg.Result()

	var msg aggregatedResponse
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("aggregated body is not valid JSON: %v", err)
	}
	if msg.ReasoningContent != "Let me think step by step." {
		t.Errorf("reasoning_content = %q, want %q", msg.ReasoningContent, "Let me think step by step.")
	}
	if msg.Content != "The answer is 42." {
		t.Errorf("content = %q, want %q", msg.Content, "The answer is 42.")
	}
	if msg.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", msg.FinishReason, "stop")
	}
}

func TestSSEAggregator_ToolCalls(t *testing.T) {
	agg := NewSSEAggregator()

	chunks := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hello\"}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"calculator","arguments":"{}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}

	for _, chunk := range chunks {
		agg.ProcessChunk([]byte(chunk))
	}

	body, _, _, _, toolNames := agg.Result()

	if len(toolNames) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(toolNames))
	}
	if toolNames[0] != "search" || toolNames[1] != "calculator" {
		t.Errorf("tool names = %v, expected [search calculator]", toolNames)
	}

	var msg aggregatedResponse
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("aggregated body is not valid JSON: %v", err)
	}
	if msg.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want %q", msg.FinishReason, "tool_calls")
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 aggregated tool calls, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].ID != "call_1" || msg.ToolCalls[0].Name != "search" || msg.ToolCalls[0].Arguments != `{"query":"hello"}` {
		t.Errorf("tool call 0 = %+v, want id=call_1 name=search arguments={\"query\":\"hello\"}", msg.ToolCalls[0])
	}
	if msg.ToolCalls[1].ID != "call_2" || msg.ToolCalls[1].Name != "calculator" || msg.ToolCalls[1].Arguments != "{}" {
		t.Errorf("tool call 1 = %+v", msg.ToolCalls[1])
	}
}

// feedSSE feeds a stream in fixed-size chunks to exercise arbitrary chunk
// boundaries, including cuts in the middle of an SSE line.
func feedSSE(agg *SSEAggregator, stream string, chunkSize int) {
	for i := 0; i < len(stream); i += chunkSize {
		end := i + chunkSize
		if end > len(stream) {
			end = len(stream)
		}
		agg.ProcessChunk([]byte(stream[i:end]))
	}
}

func TestSSEAggregator_SplitLinesAcrossChunks(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"content":"Hello"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"thinking..."}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":" world"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":1}}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	// Chunk size 1 cuts every line at every possible position; the other sizes
	// exercise coarser boundaries.
	for _, chunkSize := range []int{1, 3, 17, 64} {
		agg := NewSSEAggregator()
		feedSSE(agg, stream, chunkSize)

		body, inputTok, outputTok, cacheHit, _ := agg.Result()
		var msg aggregatedResponse
		if err := json.Unmarshal([]byte(body), &msg); err != nil {
			t.Fatalf("chunk size %d: aggregated body is not valid JSON: %v", chunkSize, err)
		}
		if msg.Content != "Hello world" {
			t.Errorf("chunk size %d: content = %q, want %q", chunkSize, msg.Content, "Hello world")
		}
		if msg.ReasoningContent != "thinking..." {
			t.Errorf("chunk size %d: reasoning_content = %q", chunkSize, msg.ReasoningContent)
		}
		if msg.FinishReason != "stop" {
			t.Errorf("chunk size %d: finish_reason = %q", chunkSize, msg.FinishReason)
		}
		if inputTok != 7 || outputTok != 3 || cacheHit != 1 {
			t.Errorf("chunk size %d: usage = %d/%d/%d, want 7/3/1", chunkSize, inputTok, outputTok, cacheHit)
		}
	}
}

func TestSSEAggregator_CRLFAndTrailingLineWithoutNewline(t *testing.T) {
	// CRLF line endings and a final event without trailing newline must both
	// be parsed.
	stream := `data: {"choices":[{"index":0,"delta":{"content":"crlf"}}]}` + "\r\n\r\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\r\n\r\n" +
		`data: [DONE]`

	agg := NewSSEAggregator()
	feedSSE(agg, stream, 5)

	body, _, _, _, _ := agg.Result()
	var msg aggregatedResponse
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("aggregated body is not valid JSON: %v", err)
	}
	if msg.Content != "crlf" {
		t.Errorf("content = %q, want %q", msg.Content, "crlf")
	}
	if msg.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", msg.FinishReason, "stop")
	}
}

func TestSSEAggregator_LargeStream(t *testing.T) {
	agg := NewSSEAggregator()

	// 11MB of aggregated content: previously anything beyond the 10MB raw
	// stream limit was dropped; the aggregated result must now be complete.
	const chunkCount = 11
	const chunkSize = 1024 * 1024
	payload := strings.Repeat("x", chunkSize)
	for i := 0; i < chunkCount; i++ {
		agg.ProcessChunk([]byte(`data: {"choices":[{"index":0,"delta":{"content":"` + payload + `"}}]}` + "\n\n"))
	}
	agg.ProcessChunk([]byte(`data: [DONE]` + "\n\n"))

	body, _, _, _, _ := agg.Result()
	var msg aggregatedResponse
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("aggregated body is not valid JSON: %v", err)
	}
	if len(msg.Content) != chunkCount*chunkSize {
		t.Errorf("content length = %d, want %d", len(msg.Content), chunkCount*chunkSize)
	}
}

func TestParseUsageFromResponse(t *testing.T) {
	body := `{
		"id": "chatcmpl-1",
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "hello",
				"tool_calls": [
					{"function": {"name": "get_weather"}},
					{"function": {"name": "search"}}
				]
			}
		}],
		"usage": {
			"prompt_tokens": 50,
			"completion_tokens": 30,
			"prompt_tokens_details": {"cached_tokens": 10}
		}
	}`

	input, output, cache, tools := ParseUsageFromResponse([]byte(body))

	if input != 50 {
		t.Errorf("input_tokens = %d, want 50", input)
	}
	if output != 30 {
		t.Errorf("output_tokens = %d, want 30", output)
	}
	if cache != 10 {
		t.Errorf("cache_hit_tokens = %d, want 10", cache)
	}
	if len(tools) != 2 {
		t.Errorf("tool count = %d, want 2", len(tools))
	}
}

func TestStreamForwarder(t *testing.T) {
	// Simulate upstream SSE data
	upstreamData := `data: {"choices":[{"index":0,"delta":{"content":"Hi"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":" there"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":0}}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	upstream := bytes.NewReader([]byte(upstreamData))
	var client bytes.Buffer

	sf := NewStreamForwarder(upstream, &client, nil)
	if err := sf.Run(); err != nil {
		t.Fatalf("stream forwarder error: %v", err)
	}

	// Client should have received the raw data
	if client.String() != upstreamData {
		t.Error("client did not receive exact upstream data")
	}

	// Aggregator should have extracted usage and aggregated content
	body, input, output, _, _ := sf.Aggregator().Result()
	if input != 5 {
		t.Errorf("aggregated input_tokens = %d, want 5", input)
	}
	if output != 3 {
		t.Errorf("aggregated output_tokens = %d, want 3", output)
	}
	var msg aggregatedResponse
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("aggregated body is not valid JSON: %v", err)
	}
	if msg.Content != "Hi there" {
		t.Errorf("aggregated content = %q, want %q", msg.Content, "Hi there")
	}
}

func TestStreamForwarder_SplitAcrossReadBuffer(t *testing.T) {
	// A single SSE line with a payload larger than the 32KB read buffer forces
	// the forwarder to hand the aggregator chunks that split the line mid-JSON.
	bigContent := strings.Repeat("A", 40*1024) // 40KB > 32KB read buffer
	upstreamData := `data: {"choices":[{"index":0,"delta":{"content":"` + bigContent + `"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":" end"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	upstream := bytes.NewReader([]byte(upstreamData))
	var client bytes.Buffer

	sf := NewStreamForwarder(upstream, &client, nil)
	if err := sf.Run(); err != nil {
		t.Fatalf("stream forwarder error: %v", err)
	}
	if client.String() != upstreamData {
		t.Error("client did not receive exact upstream data")
	}

	body, _, _, _, _ := sf.Aggregator().Result()
	var msg aggregatedResponse
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("aggregated body is not valid JSON: %v", err)
	}
	if msg.Content != bigContent+" end" {
		t.Errorf("aggregated content length = %d, want %d", len(msg.Content), len(bigContent)+4)
	}
	if msg.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", msg.FinishReason, "stop")
	}
}

func TestSSEAggregator_SawEvent(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"json error payload", `{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`, false},
		{"html error page", "<html><body>502 Bad Gateway</body></html>", false},
		{"plain text", "rate limited", false},
		{"sse stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n", true},
		{"sse done only", "data: [DONE]\n\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agg := NewSSEAggregator()
			agg.ProcessChunk([]byte(tc.body))
			agg.Result() // flushes a trailing line without a newline
			if got := agg.SawEvent(); got != tc.want {
				t.Errorf("SawEvent() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSSEAggregator_PendingLineCap(t *testing.T) {
	agg := NewSSEAggregator()

	// A single line growing past the cap without a newline is dropped so the
	// aggregator's memory stays bounded.
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 17; i++ {
		agg.ProcessChunk(chunk)
	}
	if agg.pending.Len() != 0 {
		t.Errorf("pending len = %d, want 0 (line dropped past cap)", agg.pending.Len())
	}

	// The aggregator stays usable for subsequent well-formed events.
	agg.ProcessChunk([]byte(`data: {"choices":[{"index":0,"delta":{"content":"after-cap"}}]}` + "\n\n"))
	agg.Result()
	if !strings.Contains(agg.content.String(), "after-cap") {
		t.Error("aggregator did not recover after dropping an oversized line")
	}
}

func TestStreamForwarder_RawFallbackCapture(t *testing.T) {
	// Non-SSE bodies are captured verbatim for the audit fallback.
	body := []byte(`{"error":{"message":"upstream overloaded","code":529}}`)
	var client bytes.Buffer
	sf := NewStreamForwarder(bytes.NewReader(body), &client, nil)
	if err := sf.Run(); err != nil {
		t.Fatal(err)
	}
	if got := string(sf.Raw()); got != string(body) {
		t.Errorf("Raw() = %q, want %q", got, body)
	}
	if sf.Aggregator().SawEvent() {
		t.Error("SawEvent() = true for a non-SSE body")
	}
	if client.String() != string(body) {
		t.Error("client did not receive the full body")
	}

	// Oversized bodies are capped at maxRawFallbackSize.
	big := bytes.Repeat([]byte("x"), maxRawFallbackSize+4096)
	var sink bytes.Buffer
	sf = NewStreamForwarder(bytes.NewReader(big), &sink, nil)
	if err := sf.Run(); err != nil {
		t.Fatal(err)
	}
	if len(sf.Raw()) != maxRawFallbackSize {
		t.Errorf("len(Raw()) = %d, want %d", len(sf.Raw()), maxRawFallbackSize)
	}
}

// --- Mock store for testing ---

type mockStore struct {
	mu    sync.Mutex
	count int
	delay time.Duration
}

func (m *mockStore) Insert(_ Record) error {
	m.mu.Lock()
	m.count++
	m.mu.Unlock()
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	return nil
}

func (m *mockStore) Close() error { return nil }
