package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

const maxResponseBodySize = 10 * 1024 * 1024 // 10MB

// SSEAggregator collects SSE chunks and assembles a complete response for audit.
type SSEAggregator struct {
	buf         bytes.Buffer
	content     strings.Builder
	toolCalls   map[int]*toolCallAccum
	usage       *usageInfo
	truncated   bool
	statusCode  int
	firstByteAt bool
}

type toolCallAccum struct {
	ID      string
	Type    string
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

// ProcessChunk reads an SSE event data line and extracts content for aggregation.
// It also writes the raw chunk to the internal buffer for raw storage.
func (a *SSEAggregator) ProcessChunk(data []byte) {
	// Accumulate raw data (with truncation guard)
	if a.buf.Len()+len(data) <= maxResponseBodySize {
		a.buf.Write(data)
	} else if !a.truncated {
		a.truncated = true
	}

	// Parse SSE events from data
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}

		var chunk sseChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}

		// Extract usage (often in the last chunk)
		if chunk.Usage != nil {
			a.usage = chunk.Usage
		}

		// Merge choices
		for _, choice := range chunk.Choices {
			delta := choice.Delta

			// Accumulate content
			if delta.Content != "" {
				a.content.WriteString(delta.Content)
			}

			// Accumulate tool calls
			for _, tc := range delta.ToolCalls {
				idx := tc.Index
				acc, ok := a.toolCalls[idx]
				if !ok {
					acc = &toolCallAccum{
						ID:   tc.ID,
						Type: tc.Type,
						Name: tc.Function.Name,
					}
					a.toolCalls[idx] = acc
				}
				if tc.Function.Name != "" && acc.Name == "" {
					acc.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.ArgsBuf.WriteString(tc.Function.Arguments)
				}
			}
		}
	}
}

// Result returns the aggregated response body and metadata.
func (a *SSEAggregator) Result() (responseBody string, truncated bool, inputTokens, outputTokens, cacheHitTokens int, toolCallNames []string) {
	responseBody = a.buf.String()
	truncated = a.truncated

	if a.usage != nil {
		inputTokens = a.usage.PromptTokens
		outputTokens = a.usage.CompletionTokens
		if a.usage.PromptTokensDetails != nil {
			cacheHitTokens = a.usage.PromptTokensDetails.CachedTokens
		}
	}

	// Collect tool call names
	names := make([]string, 0, len(a.toolCalls))
	for _, acc := range a.toolCalls {
		if acc.Name != "" {
			names = append(names, acc.Name)
		}
	}

	return responseBody, truncated, inputTokens, outputTokens, cacheHitTokens, names
}

// --- SSE chunk parsing types ---

type sseChunk struct {
	Choices []sseChoice `json:"choices"`
	Usage   *usageInfo  `json:"usage"`
}

type sseChoice struct {
	Index int      `json:"index"`
	Delta sseDelta `json:"delta"`
}

type sseDelta struct {
	Content   string        `json:"content"`
	ToolCalls []sseToolCall `json:"tool_calls"`
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

// StreamForwarder wraps an upstream response body, forwarding each byte
// to the client writer while simultaneously feeding the aggregator.
type StreamForwarder struct {
	upstream   io.Reader
	client     io.Writer
	aggregator *SSEAggregator
	flusher    interface{ Flush() }
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

// Run reads from upstream, forwards to client, and aggregates.
// Returns when upstream is exhausted or an error occurs.
func (sf *StreamForwarder) Run() error {
	buf := make([]byte, 32*1024) // 32KB read buffer

	for {
		n, err := sf.upstream.Read(buf)
		if n > 0 {
			chunk := buf[:n]

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
