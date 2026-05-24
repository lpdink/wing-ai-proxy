package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Context keys for gin.
const (
	CtxRequestID = "request_id"
	CtxLogger    = "logger"
	CtxAPIKey    = "virtual_api_key"
)

// RequestID generates a UUID for each request and injects it into context + response header.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := uuid.New().String()
		c.Set(CtxRequestID, id)
		c.Header("X-Request-Id", id)

		// Create a request-scoped logger
		logger := slog.With("request_id", id)
		c.Set(CtxLogger, logger)

		c.Next()
	}
}

// GetRequestID retrieves the request ID from gin context.
func GetRequestID(c *gin.Context) string {
	if v, ok := c.Get(CtxRequestID); ok {
		return v.(string)
	}
	return ""
}

// GetLogger retrieves the request-scoped logger from gin context.
func GetLogger(c *gin.Context) *slog.Logger {
	if v, ok := c.Get(CtxLogger); ok {
		return v.(*slog.Logger)
	}
	return slog.Default()
}

// Auth validates the Bearer token against a dynamic set of virtual API keys.
type Auth struct {
	mu   sync.RWMutex
	keys map[string]bool
}

// NewAuth creates an Auth middleware with the given keys.
func NewAuth(keys []string) *Auth {
	a := &Auth{keys: make(map[string]bool)}
	for _, k := range keys {
		a.keys[k] = true
	}
	return a
}

// UpdateKeys replaces the valid key set atomically.
func (a *Auth) UpdateKeys(keys []string) {
	newKeys := make(map[string]bool, len(keys))
	for _, k := range keys {
		newKeys[k] = true
	}
	a.mu.Lock()
	a.keys = newKeys
	a.mu.Unlock()
}

// Handler returns the gin middleware function.
func (a *Auth) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing Authorization header"})
			c.Abort()
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == authHeader { // no "Bearer " prefix
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid Authorization format, expected Bearer token"})
			c.Abort()
			return
		}

		a.mu.RLock()
		valid := a.keys[token]
		a.mu.RUnlock()

		if !valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
			c.Abort()
			return
		}

		c.Set(CtxAPIKey, token)
		c.Next()
	}
}
