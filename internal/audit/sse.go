package audit

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
)

// maxPendingLineSize bounds the bytes kept for a single partial SSE line.
// Real events are small; a line that grows beyond this without a newline
// signals upstream misbehavior, and the partial line is dropped to keep the
// aggregator's memory bounded.
const maxPendingLineSize = 16 << 20 // 16MB

// SSEAggregator collects SSE chunks and assembles the aggregated response
// message for audit. The raw SSE stream is not stored: only content,
// reasoning content, tool calls, finish reason and usage are retained and
// rendered as a compact JSON document by Result. Multiple upstream choices
// (n > 1) are merged into a single message, which is a known limitation.
type SSEAggregator struct {
	// pending buffers bytes that have not yet formed a complete line: a line
	// split across chunks is parsed once its remainder arrives. A bytes.Buffer
	// keeps appends amortized (O(total)) instead of re-allocating per chunk.
	pending      bytes.Buffer
	content      strings.Builder
	reasoning    strings.Builder
	toolCalls    map[int]*toolCallAccum
	finishReason string
	usage        *usageInfo
	// sawData records whether at least one SSE data line was seen. Handlers
	// use it to detect non-SSE upstream bodies (JSON error payloads, HTML
	// error pages) and fall back to storing the raw bytes.
	sawData bool
}

type toolCallAccum struct {
	ID      string
	Name    string
	ArgsBuf strings.Builder
}

type usageInfo struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details"`
}

type promptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// NewSSEAggregator creates a new aggregator.
func NewSSEAggregator() *SSEAggregator {
	return &SSEAggregator{
		toolCalls: make(map[int]*toolCallAccum),
	}
}

// ProcessChunk feeds a chunk of raw upstream bytes to the aggregator. Chunk
// boundaries are arbitrary: a line split across chunks is carried over and
// parsed once its remainder arrives.
func (a *SSEAggregator) ProcessChunk(data []byte) {
	a.pending.Write(data)
	a.drainLines()
	if a.pending.Len() > maxPendingLineSize {
		a.pending.Reset()
	}
}

// drainLines parses every complete line currently buffered.
func (a *SSEAggregator) drainLines() {
	for {
		buf := a.pending.Bytes()
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			return
		}
		a.processLine(buf[:idx])
		a.pending.Next(idx + 1) // consume the line without copying
	}
}

// processLine parses a single SSE line. Non-data lines and malformed payloads
// are ignored.
func (a *SSEAggregator) processLine(line []byte) {
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	a.sawData = true
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}

	var chunk sseChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}

	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}

	for _, choice := range chunk.Choices {
		if choice.FinishReason != "" {
			a.finishReason = choice.FinishReason
		}

		if choice.Delta.Content != "" {
			a.content.WriteString(choice.Delta.Content)
		}
		if choice.Delta.ReasoningContent != "" {
			a.reasoning.WriteString(choice.Delta.ReasoningContent)
		}

		for _, tc := range choice.Delta.ToolCalls {
			acc, ok := a.toolCalls[tc.Index]
			if !ok {
				acc = &toolCallAccum{}
				a.toolCalls[tc.Index] = acc
			}
			if acc.ID == "" {
				acc.ID = tc.ID
			}
			if acc.Name == "" {
				acc.Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				acc.ArgsBuf.WriteString(tc.Function.Arguments)
			}
		}
	}
}

// finish parses a possible trailing line that was not terminated by a newline.
func (a *SSEAggregator) finish() {
	if a.pending.Len() == 0 {
		return
	}
	a.processLine(a.pending.Bytes())
	a.pending.Reset()
}

// SawEvent reports whether the stream contained at least one SSE data line.
// When it did not, the upstream body was not an SSE stream at all (a JSON
// error payload, an HTML error page, ...) and callers should fall back to
// storing the raw bytes instead of an empty aggregate.
func (a *SSEAggregator) SawEvent() bool {
	return a.sawData
}

// Result returns the aggregated response body as JSON, plus token usage and
// the names of the tool calls seen in the stream. It must only be called after
// the stream has been fully processed.
func (a *SSEAggregator) Result() (responseBody string, inputTokens, outputTokens, cacheHitTokens int, toolCallNames []string) {
	a.finish()

	if a.usage != nil {
		inputTokens = a.usage.PromptTokens
		outputTokens = a.usage.CompletionTokens
		if a.usage.PromptTokensDetails != nil {
			cacheHitTokens = a.usage.PromptTokensDetails.CachedTokens
		}
	}

	indexes := make([]int, 0, len(a.toolCalls))
	for idx := range a.toolCalls {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)

	toolCalls := make([]aggregatedToolCall, 0, len(indexes))
	toolCallNames = make([]string, 0, len(indexes))
	for _, idx := range indexes {
		acc := a.toolCalls[idx]
		if acc.Name != "" {
			toolCallNames = append(toolCallNames, acc.Name)
		}
		toolCalls = append(toolCalls, aggregatedToolCall{
			ID:        acc.ID,
			Name:      acc.Name,
			Arguments: acc.ArgsBuf.String(),
		})
	}

	body := aggregatedResponse{
		Content:          a.content.String(),
		ReasoningContent: a.reasoning.String(),
		ToolCalls:        toolCalls,
		FinishReason:     a.finishReason,
		Usage: aggregatedUsage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			CachedTokens:     cacheHitTokens,
		},
	}

	out, _ := json.Marshal(body) // fixed struct of strings/ints: cannot fail
	return string(out), inputTokens, outputTokens, cacheHitTokens, toolCallNames
}

// aggregatedResponse is the audit body persisted for streaming responses. It
// approximates the chat.completion message shape without pretending to be a
// full API response.
type aggregatedResponse struct {
	Content          string               `json:"content"`
	ReasoningContent string               `json:"reasoning_content"`
	ToolCalls        []aggregatedToolCall `json:"tool_calls"`
	FinishReason     string               `json:"finish_reason"`
	Usage            aggregatedUsage      `json:"usage"`
}

type aggregatedToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type aggregatedUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}

// --- SSE chunk parsing types ---

type sseChunk struct {
	Choices []sseChoice `json:"choices"`
	Usage   *usageInfo  `json:"usage"`
}

type sseChoice struct {
	Index        int      `json:"index"`
	Delta        sseDelta `json:"delta"`
	FinishReason string   `json:"finish_reason"`
}

type sseDelta struct {
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content"`
	ToolCalls        []sseToolCall `json:"tool_calls"`
}

type sseToolCall struct {
	Index    int         `json:"index"`
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function sseFunction `json:"function"`
}

type sseFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// maxRawFallbackSize bounds the leading raw bytes of the upstream body kept
// for the non-SSE fallback (error payloads, HTML error pages). SSE streams
// are aggregated and never stored raw.
const maxRawFallbackSize = 1 << 20 // 1MB

// StreamForwarder wraps an upstream response body, forwarding each byte
// to the client writer while simultaneously feeding the aggregator. It also
// keeps the leading upstream bytes (up to maxRawFallbackSize) so that callers
// can store the raw body when the response turns out not to be an SSE stream.
type StreamForwarder struct {
	upstream   io.Reader
	client     io.Writer
	aggregator *SSEAggregator
	flusher    interface{ Flush() }
	raw        []byte
}

// NewStreamForwarder creates a new stream forwarder.
func NewStreamForwarder(upstream io.Reader, client io.Writer, flusher interface{ Flush() }) *StreamForwarder {
	return &StreamForwarder{
		upstream:   upstream,
		client:     client,
		aggregator: NewSSEAggregator(),
		flusher:    flusher,
	}
}

// Aggregator returns the SSE aggregator for accessing results after streaming.
func (sf *StreamForwarder) Aggregator() *SSEAggregator {
	return sf.aggregator
}

// Raw returns the leading bytes of the upstream body (up to
// maxRawFallbackSize). Callers use it as the audit fallback when the response
// was not an SSE stream.
func (sf *StreamForwarder) Raw() []byte {
	return sf.raw
}

// Run reads from upstream, forwards to client, and aggregates.
// Returns when upstream is exhausted or an error occurs.
func (sf *StreamForwarder) Run() error {
	buf := make([]byte, 32*1024) // 32KB read buffer

	for {
		n, err := sf.upstream.Read(buf)
		if n > 0 {
			chunk := buf[:n]

			// Keep the leading bytes for the non-SSE audit fallback.
			if remaining := maxRawFallbackSize - len(sf.raw); remaining > 0 {
				if remaining > len(chunk) {
					remaining = len(chunk)
				}
				sf.raw = append(sf.raw, chunk[:remaining]...)
			}

			// Forward to client immediately
			if _, werr := sf.client.Write(chunk); werr != nil {
				return werr
			}
			if sf.flusher != nil {
				sf.flusher.Flush()
			}

			// Feed to aggregator
			sf.aggregator.ProcessChunk(chunk)
		}

		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// ParseUsageFromResponse extracts usage info from a non-streaming response body.
func ParseUsageFromResponse(body []byte) (inputTokens, outputTokens, cacheHitTokens int, toolCallNames []string) {
	var resp struct {
		Usage   *usageInfo `json:"usage"`
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, 0, 0, nil
	}

	if resp.Usage != nil {
		inputTokens = resp.Usage.PromptTokens
		outputTokens = resp.Usage.CompletionTokens
		if resp.Usage.PromptTokensDetails != nil {
			cacheHitTokens = resp.Usage.PromptTokensDetails.CachedTokens
		}
	}

	for _, choice := range resp.Choices {
		for _, tc := range choice.Message.ToolCalls {
			if tc.Function.Name != "" {
				toolCallNames = append(toolCallNames, tc.Function.Name)
			}
		}
	}

	return
}
