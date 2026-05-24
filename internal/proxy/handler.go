package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/abiter/wing-ai-proxy/internal/audit"
	"github.com/abiter/wing-ai-proxy/internal/metrics"
	"github.com/abiter/wing-ai-proxy/internal/middleware"
	"github.com/abiter/wing-ai-proxy/internal/provider"
)

// Handler handles proxy requests.
type Handler struct {
	registry      *provider.Registry
	auditWriter   *audit.AsyncWriter
}

// NewHandler creates a new proxy handler.
func NewHandler(registry *provider.Registry, auditWriter *audit.AsyncWriter) *Handler {
	return &Handler{
		registry:    registry,
		auditWriter: auditWriter,
	}
}

// Models handles GET /models.
func (h *Handler) Models(c *gin.Context) {
	models := h.registry.AllModels()

	data := make([]gin.H, 0, len(models))
	for _, m := range models {
		data = append(data, gin.H{
			"id":       m.ID,
			"object":   m.Object,
			"owned_by": m.OwnedBy,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   data,
	})
}

// ChatCompletions handles POST /chat/completions.
func (h *Handler) ChatCompletions(c *gin.Context) {
	logger := middleware.GetLogger(c)
	requestID := middleware.GetRequestID(c)
	startTime := time.Now()

	// Read request body
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.Error("failed to read request body", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	// Extract model from request
	var reqBody struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &reqBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	// Resolve provider and real model name
	p, realName, err := h.registry.Resolve(reqBody.Model)
	if err != nil {
		availableModels := h.registry.ShowNames()
		c.JSON(http.StatusBadRequest, gin.H{
			"error":           fmt.Sprintf("model %q not found", reqBody.Model),
			"available_models": availableModels,
		})
		return
	}

	logger = logger.With("provider", p.Name(), "model_show", reqBody.Model, "model_real", realName)

	// Get virtual API key from context
	virtualKey, _ := c.Get(middleware.CtxAPIKey)

	// Base audit record
	auditRecord := audit.Record{
		RequestID:     requestID,
		VirtualAPIKey: virtualKey.(string),
		ProviderName:  p.Name(),
		ModelShowName: reqBody.Model,
		ModelRealName: realName,
		RequestStart:  startTime,
		IsStream:      reqBody.Stream,
		RequestBody:   string(body),
	}

	if reqBody.Stream {
		h.handleStream(c, p, body, c.Request.Header, &auditRecord, logger, startTime)
	} else {
		h.handleNonStream(c, p, body, c.Request.Header, &auditRecord, logger, startTime)
	}
}

func (h *Handler) handleNonStream(
	c *gin.Context,
	p provider.Provider,
	body []byte,
	headers http.Header,
	ar *audit.Record,
	logger *slog.Logger,
	startTime time.Time,
) {
	ctx := c.Request.Context()

	resp, err := p.ChatCompletion(ctx, body, headers)
	if err != nil {
		h.handleUpstreamError(c, err, ar, logger, startTime)
		return
	}
	defer resp.Body.Close()

	ar.FirstByteAt = time.Now()
	ar.StatusCode = resp.StatusCode

	// Read full response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("failed to read upstream response", "error", err)
		ar.ErrorMessage = fmt.Sprintf("read response: %v", err)
		ar.RequestEnd = time.Now()
		h.recordMetrics(ar, startTime)
		h.auditWriter.Submit(*ar)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read upstream response"})
		return
	}

	ar.ResponseBody = string(respBody)
	ar.RequestEnd = time.Now()

	// Parse usage and tool calls from response
	if resp.StatusCode == 200 {
		inputTok, outputTok, cacheHit, toolCalls := audit.ParseUsageFromResponse(respBody)
		ar.InputTokens = inputTok
		ar.OutputTokens = outputTok
		ar.CacheHitTokens = cacheHit
		ar.ToolCalls = toolCalls
	}

	h.recordMetrics(ar, startTime)
	h.auditWriter.Submit(*ar)

	// Forward response headers
	for key, vals := range resp.Header {
		for _, v := range vals {
			c.Header(key, v)
		}
	}
	c.Status(resp.StatusCode)
	c.Writer.Write(respBody)
}

func (h *Handler) handleStream(
	c *gin.Context,
	p provider.Provider,
	body []byte,
	headers http.Header,
	ar *audit.Record,
	logger *slog.Logger,
	startTime time.Time,
) {
	ctx := c.Request.Context()

	resp, err := p.ChatCompletion(ctx, body, headers)
	if err != nil {
		h.handleUpstreamError(c, err, ar, logger, startTime)
		return
	}
	defer resp.Body.Close()

	ar.StatusCode = resp.StatusCode
	ar.FirstByteAt = time.Now()

	// Forward response headers for SSE
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	for key, vals := range resp.Header {
		if key == "Content-Type" || key == "Cache-Control" || key == "Connection" {
			continue
		}
		for _, v := range vals {
			c.Header(key, v)
		}
	}
	c.Status(resp.StatusCode)

	// Safe flusher assertion — Gin's responseWriter supports Flush, but guard anyway.
	flusher, _ := c.Writer.(interface{ Flush() })
	if flusher == nil {
		logger.Warn("response writer does not support flushing")
	}

	// Use stream forwarder to forward chunks to client and aggregate for audit
	forwarder := audit.NewStreamForwarder(resp.Body, c.Writer, flusher)
	if err := forwarder.Run(); err != nil {
		logger.Error("stream forwarding error", "error", err)
		ar.ErrorMessage = fmt.Sprintf("stream: %v", err)
	}

	ar.RequestEnd = time.Now()

	// Get aggregated results
	respBody, truncated, inputTok, outputTok, cacheHit, toolCalls := forwarder.Aggregator().Result()
	ar.ResponseBody = respBody
	ar.Truncated = truncated
	ar.InputTokens = inputTok
	ar.OutputTokens = outputTok
	ar.CacheHitTokens = cacheHit
	ar.ToolCalls = toolCalls

	h.recordMetrics(ar, startTime)
	h.auditWriter.Submit(*ar)

	logger.Info("stream completed",
		"duration_ms", ar.RequestEnd.Sub(ar.RequestStart).Milliseconds(),
		"input_tokens", inputTok,
		"output_tokens", outputTok,
	)
}

func (h *Handler) handleUpstreamError(
	c *gin.Context,
	err error,
	ar *audit.Record,
	logger *slog.Logger,
	startTime time.Time,
) {
	ar.RequestEnd = time.Now()
	ar.ErrorMessage = err.Error()

	if provider.IsTimeoutError(err) {
		ar.StatusCode = http.StatusGatewayTimeout
		logger.Error("upstream timeout", "error", err)
		h.recordMetrics(ar, startTime)
		h.auditWriter.Submit(*ar)
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "upstream timeout"})
		return
	}

	ar.StatusCode = http.StatusBadGateway
	logger.Error("upstream error", "error", err)
	h.recordMetrics(ar, startTime)
	h.auditWriter.Submit(*ar)
	c.JSON(http.StatusBadGateway, gin.H{"error": "upstream error: " + err.Error()})
}

func (h *Handler) recordMetrics(ar *audit.Record, startTime time.Time) {
	duration := ar.RequestEnd.Sub(startTime).Seconds()
	statusStr := strconv.Itoa(ar.StatusCode)

	metrics.RequestsTotal.WithLabelValues(ar.ProviderName, ar.ModelShowName, statusStr).Inc()
	metrics.RequestDuration.WithLabelValues(ar.ProviderName, ar.ModelShowName).Observe(duration)

	if !ar.FirstByteAt.IsZero() {
		ttfb := ar.FirstByteAt.Sub(startTime).Seconds()
		metrics.TimeToFirstByte.WithLabelValues(ar.ProviderName, ar.ModelShowName).Observe(ttfb)
	}

	if ar.StatusCode >= 400 || ar.ErrorMessage != "" {
		metrics.UpstreamFailures.WithLabelValues(ar.ProviderName, ar.ModelShowName).Inc()
	}

	if h.auditWriter != nil {
		metrics.AuditQueueLength.Set(float64(h.auditWriter.QueueLen()))
	}
}

// Health handles GET /health.
func Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
