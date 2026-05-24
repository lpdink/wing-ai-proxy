package audit

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migration.sql
var migrationSQL string

// Store defines the audit storage interface.
type Store interface {
	Insert(record Record) error
	Close() error
}

// SQLiteStore implements Store using SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates a new SQLite audit store, running migrations on startup.
func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	// Ensure the directory exists
	dir := filepath.Dir(dsn)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("audit: create db directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dsn+"?_journal=WAL&_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("audit: open sqlite: %w", err)
	}

	// Run migration
	if _, err := db.Exec(migrationSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit: migration: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

// Insert writes an audit record to SQLite.
func (s *SQLiteStore) Insert(r Record) error {
	toolCallsJSON, err := json.Marshal(r.ToolCalls)
	if err != nil {
		toolCallsJSON = []byte("[]")
	}

	isStream := 0
	if r.IsStream {
		isStream = 1
	}
	truncated := 0
	if r.Truncated {
		truncated = 1
	}

	_, err = s.db.Exec(`
		INSERT INTO audit_records (
			request_id, virtual_api_key, provider_name, model_show_name, model_real_name,
			request_start, first_byte_at, request_end,
			input_tokens, output_tokens, cache_hit_tokens,
			tool_calls, is_stream, request_body, response_body, truncated,
			status_code, error_message
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RequestID, r.VirtualAPIKey, r.ProviderName, r.ModelShowName, r.ModelRealName,
		r.RequestStart.Format(time.RFC3339Nano), r.FirstByteAt.Format(time.RFC3339Nano), r.RequestEnd.Format(time.RFC3339Nano),
		r.InputTokens, r.OutputTokens, r.CacheHitTokens,
		string(toolCallsJSON), isStream, r.RequestBody, r.ResponseBody, truncated,
		r.StatusCode, r.ErrorMessage,
	)

	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// Close closes the database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// AsyncWriter provides non-blocking audit record submission.
type AsyncWriter struct {
	store    Store
	queue    chan Record
	done     chan struct{}
	dropped  int64
}

// NewAsyncWriter creates an async writer with a buffered channel.
func NewAsyncWriter(store Store, bufferSize int) *AsyncWriter {
	w := &AsyncWriter{
		store: store,
		queue: make(chan Record, bufferSize),
		done:  make(chan struct{}),
	}
	go w.worker()
	return w
}

// Submit enqueues an audit record. Returns immediately; drops if queue is full.
func (w *AsyncWriter) Submit(r Record) {
	select {
	case w.queue <- r:
	default:
		w.dropped++
		slog.Warn("audit queue full, dropping record",
			"request_id", r.RequestID,
			"total_dropped", w.dropped,
		)
	}
}

// QueueLen returns the current queue length.
func (w *AsyncWriter) QueueLen() int {
	return len(w.queue)
}

// Drain processes all remaining records and stops the worker.
func (w *AsyncWriter) Drain() {
	close(w.queue)
	<-w.done
}

func (w *AsyncWriter) worker() {
	defer close(w.done)

	// Batch records for transaction efficiency
	const batchSize = 100
	batch := make([]Record, 0, batchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		for _, r := range batch {
			if err := w.store.Insert(r); err != nil {
				slog.Error("audit insert failed", "request_id", r.RequestID, "error", err)
			}
		}
		batch = batch[:0]
	}

	// Use a ticker to periodically flush even if batch isn't full
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case r, ok := <-w.queue:
			if !ok {
				// Channel closed, flush remaining
				flush()
				return
			}
			batch = append(batch, r)
			if len(batch) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}
