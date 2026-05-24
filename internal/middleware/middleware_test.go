package middleware

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestRequestID(t *testing.T) {
	r := gin.New()
	r.Use(RequestID())
	r.GET("/test", func(c *gin.Context) {
		id := GetRequestID(c)
		c.String(200, id)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	// Check X-Request-Id header
	reqID := w.Header().Get("X-Request-Id")
	if reqID == "" {
		t.Error("X-Request-Id header missing")
	}
	if len(reqID) != 36 { // UUID format
		t.Errorf("request_id length = %d, want 36", len(reqID))
	}

	// Body should contain the same ID
	if w.Body.String() != reqID {
		t.Errorf("body = %q, header = %q", w.Body.String(), reqID)
	}
}

func TestRequestID_UniquePerRequest(t *testing.T) {
	r := gin.New()
	r.Use(RequestID())
	r.GET("/test", func(c *gin.Context) {
		c.String(200, GetRequestID(c))
	})

	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		r.ServeHTTP(w, req)
		id := w.Body.String()
		if ids[id] {
			t.Fatalf("duplicate request_id: %s", id)
		}
		ids[id] = true
	}
}

func TestAuth_ValidKey(t *testing.T) {
	auth := NewAuth([]string{"sk-test-1", "sk-test-2"})

	r := gin.New()
	r.Use(RequestID(), auth.Handler())
	r.GET("/test", func(c *gin.Context) {
		key, _ := c.Get(CtxAPIKey)
		c.String(200, key.(string))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer sk-test-1")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "sk-test-1" {
		t.Errorf("body = %q, want %q", w.Body.String(), "sk-test-1")
	}
}

func TestAuth_InvalidKey(t *testing.T) {
	auth := NewAuth([]string{"sk-valid"})

	r := gin.New()
	r.Use(RequestID(), auth.Handler())
	r.GET("/test", func(c *gin.Context) {
		c.String(200, "ok")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer sk-invalid")
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] == "" {
		t.Error("expected error message in response")
	}
}

func TestAuth_MissingHeader(t *testing.T) {
	auth := NewAuth([]string{"sk-valid"})

	r := gin.New()
	r.Use(RequestID(), auth.Handler())
	r.GET("/test", func(c *gin.Context) {
		c.String(200, "ok")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuth_BadFormat(t *testing.T) {
	auth := NewAuth([]string{"sk-valid"})

	r := gin.New()
	r.Use(RequestID(), auth.Handler())
	r.GET("/test", func(c *gin.Context) {
		c.String(200, "ok")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Token sk-valid") // wrong prefix
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuth_UpdateKeys(t *testing.T) {
	auth := NewAuth([]string{"sk-old"})

	r := gin.New()
	r.Use(RequestID(), auth.Handler())
	r.GET("/test", func(c *gin.Context) {
		c.String(200, "ok")
	})

	// Old key works
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer sk-old")
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("old key status = %d, want 200", w.Code)
	}

	// Update keys
	auth.UpdateKeys([]string{"sk-new"})

	// Old key no longer works
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer sk-old")
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("old key after update status = %d, want 401", w.Code)
	}

	// New key works
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer sk-new")
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("new key status = %d, want 200", w.Code)
	}
}

func TestAuth_WhitelistPaths(t *testing.T) {
	auth := NewAuth([]string{"sk-valid"})

	r := gin.New()
	r.Use(RequestID())
	// Health and metrics should bypass auth
	r.GET("/health", func(c *gin.Context) { c.String(200, "ok") })
	r.GET("/metrics", func(c *gin.Context) { c.String(200, "metrics") })
	// Protected route
	api := r.Group("/")
	api.Use(auth.Handler())
	api.GET("/models", func(c *gin.Context) { c.String(200, "models") })

	// /health without auth should work
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("health status = %d, want 200", w.Code)
	}

	// /metrics without auth should work
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/metrics", nil)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("metrics status = %d, want 200", w.Code)
	}

	// /models without auth should fail
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/models", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("models without auth status = %d, want 401", w.Code)
	}
}
