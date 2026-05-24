package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/abiter/wing-ai-proxy/internal/audit"
	"github.com/abiter/wing-ai-proxy/internal/config"
	"github.com/abiter/wing-ai-proxy/internal/middleware"
	"github.com/abiter/wing-ai-proxy/internal/provider"
	"github.com/abiter/wing-ai-proxy/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// 1. Determine config path
	configPath := config.DefaultConfigPath()
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	// Only create default config directory; don't touch custom paths.
	if configPath == config.DefaultConfigPath() {
		if err := config.EnsureConfigDir(); err != nil {
			slog.Warn("failed to create config directory", "error", err)
		}
	}

	// 2. Load configuration

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// 3. Initialize structured JSON logger
	initLogger(cfg.LogLevel)
	slog.Info("starting wing-ai-proxy",
		"host", cfg.Host,
		"port", cfg.Port,
		"config", configPath,
	)

	// 4. Build providers and registry
	providers, err := provider.BuildProviders(cfg.Providers)
	if err != nil {
		return fmt.Errorf("build providers: %w", err)
	}
	registry := provider.NewRegistry(providers)
	slog.Info("providers loaded", "count", len(providers))

	// 5. Initialize audit store and async writer
	auditStore, err := audit.NewSQLiteStore(cfg.Database.DSN)
	if err != nil {
		return fmt.Errorf("init audit store: %w", err)
	}
	auditWriter := audit.NewAsyncWriter(auditStore, 4096)
	slog.Info("audit store initialized", "dsn", cfg.Database.DSN)

	// 6. Create auth middleware
	auth := middleware.NewAuth(cfg.VirtualAPIKeys)

	// 7. Create proxy handler
	handler := proxy.NewHandler(registry, auditWriter)

	// 8. Setup Gin router
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.RequestID())

	// Public routes (no auth)
	r.GET("/health", proxy.Health)
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Protected routes
	api := r.Group("/")
	api.Use(auth.Handler())
	api.GET("/models", handler.Models)
	api.POST("/chat/completions", handler.ChatCompletions)

	// 9. Start config watcher
	watcher, err := config.NewWatcher(configPath, cfg, func(newCfg *config.Config) {
		// Rebuild providers
		newProviders, err := provider.BuildProviders(newCfg.Providers)
		if err != nil {
			slog.Error("hot reload: failed to build providers", "error", err)
			return
		}

		// Update registry atomically
		registry.Update(newProviders)

		// Update auth keys atomically
		auth.UpdateKeys(newCfg.VirtualAPIKeys)

		slog.Info("hot reload complete",
			"providers", len(newProviders),
			"api_keys", len(newCfg.VirtualAPIKeys),
		)
	})
	if err != nil {
		slog.Warn("config watcher failed to start", "error", err)
	}

	// 10. Start HTTP server
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	go func() {
		slog.Info("server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
		}
	}()

	// 11. Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	slog.Info("received signal, shutting down", "signal", sig)

	// Close config watcher
	if watcher != nil {
		watcher.Close()
		slog.Info("config watcher closed")
	}

	// Shutdown HTTP server with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}
	slog.Info("server stopped")

	// Drain audit queue
	slog.Info("draining audit queue", "pending", auditWriter.QueueLen())
	auditWriter.Drain()
	slog.Info("audit queue drained")

	// Close audit store
	if err := auditStore.Close(); err != nil {
		slog.Error("audit store close error", "error", err)
	}

	slog.Info("wing-ai-proxy stopped")
	return nil
}

func initLogger(level string) {
	var logLevel slog.Level
	switch strings.ToLower(level) {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn", "warning":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})
	slog.SetDefault(slog.New(handler))
}
