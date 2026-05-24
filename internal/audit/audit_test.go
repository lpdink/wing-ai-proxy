package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		RequestID:     "req-001",
		VirtualAPIKey: "sk-test",
		ProviderName:  "test-provider",
		ModelShowName: "gpt-4",
		ModelRealName: "gpt-4-turbo",
		RequestStart:  time.Now().Add(-2 * time.Second),
		FirstByteAt:   time.Now().Add(-1 * time.Second),
		RequestEnd:    time.Now(),
		InputTokens:   100,
		OutputTokens:  200,
		CacheHitTokens: 50,
		ToolCalls:     []string{"search", "calculator"},
		IsStream:      false,
		RequestBody:   `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`,
		ResponseBody:  `{"id":"chatcmpl-1"}`,
		StatusCode:    200,
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
		`data: {"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_cache_hit_tokens":0}}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}

	for _, chunk := range chunks {
		agg.ProcessChunk([]byte(chunk))
	}

	body, truncated, inputTok, outputTok, _, toolNames := agg.Result()

	if truncated {
		t.Error("should not be truncated")
	}
	if inputTok != 10 {
		t.Errorf("input_tokens = %d, want 10", inputTok)
	}
	if outputTok != 5 {
		t.Errorf("output_tokens = %d, want 5", outputTok)
	}
	if len(toolNames) != 0 {
		t.Errorf("unexpected tool calls: %v", toolNames)
	}
	if body == "" {
		t.Error("response body should not be empty")
	}
}

func TestSSEAggregator_ToolCalls(t *testing.T) {
	agg := NewSSEAggregator()

	chunks := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hello\"}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"calculator","arguments":"{}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":20,"completion_tokens":10,"prompt_cache_hit_tokens":0}}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}

	for _, chunk := range chunks {
		agg.ProcessChunk([]byte(chunk))
	}

	_, _, _, _, _, toolNames := agg.Result()

	if len(toolNames) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(toolNames))
	}

	nameSet := make(map[string]bool)
	for _, n := range toolNames {
		nameSet[n] = true
	}
	if !nameSet["search"] || !nameSet["calculator"] {
		t.Errorf("tool names = %v, expected search and calculator", toolNames)
	}
}

func TestSSEAggregator_Truncation(t *testing.T) {
	agg := NewSSEAggregator()

	// Feed more than 10MB of data
	bigChunk := bytes.Repeat([]byte("x"), 1024*1024) // 1MB chunks
	for i := 0; i < 12; i++ {
		agg.ProcessChunk(bigChunk)
	}

	_, truncated, _, _, _, _ := agg.Result()
	if !truncated {
		t.Error("expected truncation for > 10MB data")
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
			"prompt_cache_hit_tokens": 10
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
		`data: {"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"prompt_cache_hit_tokens":0}}` + "\n\n" +
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

	// Aggregator should have extracted usage
	_, _, input, output, _, _ := sf.Aggregator().Result()
	if input != 5 {
		t.Errorf("aggregated input_tokens = %d, want 5", input)
	}
	if output != 3 {
		t.Errorf("aggregated output_tokens = %d, want 3", output)
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
