package audit

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// insertLegacyRow inserts a historical (plain TEXT) audit row directly, as
// written by pre-compression versions of the proxy.
func insertLegacyRow(t *testing.T, store *SQLiteStore, requestID, reqBody, respBody string) {
	t.Helper()
	_, err := store.db.Exec(`
		INSERT INTO audit_records (
			request_id, virtual_api_key, provider_name, model_show_name, model_real_name,
			request_start, first_byte_at, request_end, request_body, response_body
		) VALUES (?, 'sk-test', 'prov', 'm', 'm-real',
			'2024-01-01T00:00:00Z', '2024-01-01T00:00:01Z', '2024-01-01T00:00:02Z', ?, ?)`,
		requestID, reqBody, respBody)
	if err != nil {
		t.Fatalf("insert legacy row %s: %v", requestID, err)
	}
}

func TestCompact_MixedRows(t *testing.T) {
	dir := t.TempDir()
	dsn := dir + "/test.db"
	store, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// New-style row written by this binary: already compressed on insert.
	newReq := `{"model":"gpt-4","messages":[{"role":"user","content":"new"}]}`
	if err := store.Insert(Record{
		RequestID:     "new-1",
		VirtualAPIKey: "sk-test",
		ProviderName:  "prov",
		ModelShowName: "m",
		ModelRealName: "m-real",
		RequestStart:  time.Now(),
		FirstByteAt:   time.Now(),
		RequestEnd:    time.Now(),
		RequestBody:   newReq,
		ResponseBody:  `{"choices":[]}`,
	}); err != nil {
		t.Fatal(err)
	}

	// Historical rows with mixed body contents.
	bigJSON := `{"messages":[` + strings.Repeat(`{"role":"user","content":"hello world"},`, 100) + `]}`
	bigSSE := strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"chunk\"}}]}\n\n", 200)
	type legacy struct {
		id, req, resp string
	}
	legacyRows := []legacy{
		{"legacy-both", bigJSON, bigSSE},
		{"legacy-empty", "", ""},
		{"legacy-empty-req", "", bigSSE},
		{"legacy-empty-resp", bigJSON, ""},
	}
	for _, r := range legacyRows {
		insertLegacyRow(t, store, r.id, r.req, r.resp)
	}

	// Capture the new-style BLOB as written, to prove compact leaves it alone.
	var newBlobBefore []byte
	if err := store.db.QueryRow("SELECT request_body FROM audit_records WHERE request_id = 'new-1'").Scan(&newBlobBefore); err != nil {
		t.Fatal(err)
	}

	var progressCalls int
	result, err := Compact(dsn, CompactOptions{
		BatchSize: 2, // force multiple batches
		Progress:  func(CompactStats) { progressCalls++ },
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}

	// Count expected input/output bytes for all legacy bodies.
	var wantInput, wantOutput int64
	wantRows := int64(0)
	for _, r := range legacyRows {
		updated := false
		for _, body := range []string{r.req, r.resp} {
			if body == "" {
				continue
			}
			compressed, err := gzipEncode([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			wantInput += int64(len(body))
			wantOutput += int64(len(compressed))
			updated = true
		}
		if updated {
			wantRows++
		}
	}
	if result.RowsProcessed != wantRows {
		t.Errorf("rows processed = %d, want %d", result.RowsProcessed, wantRows)
	}
	if result.InputBytes != wantInput || result.OutputBytes != wantOutput {
		t.Errorf("bytes in/out = %d/%d, want %d/%d", result.InputBytes, result.OutputBytes, wantInput, wantOutput)
	}
	if progressCalls == 0 {
		t.Error("progress callback was never called")
	}

	// All non-empty bodies must now be gzip BLOBs with equivalent content;
	// empty bodies must remain untouched empty TEXT.
	for _, r := range legacyRows {
		var reqType, respType string
		var reqRaw, respRaw []byte
		err := store.db.QueryRow(`
			SELECT typeof(request_body), request_body, typeof(response_body), response_body
			FROM audit_records WHERE request_id = ?`, r.id).
			Scan(&reqType, &reqRaw, &respType, &respRaw)
		if err != nil {
			t.Fatal(err)
		}

		if r.req == "" {
			if reqType != "text" || len(reqRaw) != 0 {
				t.Errorf("%s: empty request_body was modified (type=%s, len=%d)", r.id, reqType, len(reqRaw))
			}
		} else {
			if reqType != "blob" {
				t.Errorf("%s: typeof(request_body) = %q, want blob", r.id, reqType)
			}
			got, err := gzipDecode(reqRaw)
			if err != nil {
				t.Fatalf("%s: decode request_body: %v", r.id, err)
			}
			if string(got) != r.req {
				t.Errorf("%s: request_body content mismatch after compact", r.id)
			}
		}

		if r.resp == "" {
			if respType != "text" || len(respRaw) != 0 {
				t.Errorf("%s: empty response_body was modified (type=%s, len=%d)", r.id, respType, len(respRaw))
			}
		} else {
			if respType != "blob" {
				t.Errorf("%s: typeof(response_body) = %q, want blob", r.id, respType)
			}
			got, err := gzipDecode(respRaw)
			if err != nil {
				t.Fatalf("%s: decode response_body: %v", r.id, err)
			}
			if string(got) != r.resp {
				t.Errorf("%s: response_body content mismatch after compact", r.id)
			}
		}
	}

	// The already-compressed row must be byte-for-byte identical.
	var newBlobAfter []byte
	if err := store.db.QueryRow("SELECT request_body FROM audit_records WHERE request_id = 'new-1'").Scan(&newBlobAfter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(newBlobBefore, newBlobAfter) {
		t.Error("pre-compressed row was modified by compact")
	}

	// No legacy text bodies may remain, and a second run is a no-op.
	var remaining int
	if err := store.db.QueryRow(`
		SELECT COUNT(*) FROM audit_records
		WHERE (typeof(request_body) = 'text' AND request_body <> '')
		   OR (typeof(response_body) = 'text' AND response_body <> '')`).
		Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("%d legacy text bodies remain after compact", remaining)
	}

	second, err := Compact(dsn, CompactOptions{})
	if err != nil {
		t.Fatalf("second compact: %v", err)
	}
	if second.RowsProcessed != 0 || second.InputBytes != 0 || second.OutputBytes != 0 {
		t.Errorf("second compact not a no-op: %+v", second)
	}
}

func TestCompact_Vacuum(t *testing.T) {
	dir := t.TempDir()
	dsn := dir + "/test.db"
	store, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	body := strings.Repeat(`{"role":"user","content":"padding padding padding"}`, 300)
	for i := 0; i < 200; i++ {
		insertLegacyRow(t, store, fmt.Sprintf("legacy-vac-%03d", i), body, body)
	}

	result, err := Compact(dsn, CompactOptions{Vacuum: true})
	if err != nil {
		t.Fatalf("compact with vacuum: %v", err)
	}
	if result.RowsProcessed != 200 {
		t.Errorf("rows processed = %d, want 200", result.RowsProcessed)
	}
	if result.ReclaimedBytes < 0 {
		t.Errorf("reclaimed bytes = %d, want >= 0", result.ReclaimedBytes)
	}
}

func TestCompact_MissingDatabase(t *testing.T) {
	_, err := Compact(t.TempDir()+"/does-not-exist.db", CompactOptions{})
	if err == nil {
		t.Fatal("expected error for missing database file")
	}
}

func TestCompact_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	dsn := dir + "/concurrent.db"
	store, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	body := `{"messages":[{"role":"user","content":"some text that should compress well enough"}]}`
	const legacyRows = 200
	for i := 0; i < legacyRows; i++ {
		insertLegacyRow(t, store, fmt.Sprintf("old-%03d", i), body, body)
	}

	// Simulate a running proxy appending new (already compressed) rows while
	// compact converts the legacy rows.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			rec := Record{
				RequestID:     fmt.Sprintf("live-%05d", i),
				VirtualAPIKey: "sk-test",
				ProviderName:  "prov",
				ModelShowName: "m",
				ModelRealName: "m-real",
				RequestStart:  time.Now(),
				FirstByteAt:   time.Now(),
				RequestEnd:    time.Now(),
				RequestBody:   body,
				ResponseBody:  body,
			}
			if err := store.Insert(rec); err != nil {
				errCh <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	result, err := Compact(dsn, CompactOptions{BatchSize: 20})
	close(stop)
	wg.Wait()

	if err != nil {
		t.Fatalf("compact with concurrent writer: %v", err)
	}
	select {
	case werr := <-errCh:
		t.Fatalf("concurrent insert failed: %v", werr)
	default:
	}
	if result.RowsProcessed != legacyRows {
		t.Errorf("rows processed = %d, want %d", result.RowsProcessed, legacyRows)
	}

	var remaining int
	if err := store.db.QueryRow(`
		SELECT COUNT(*) FROM audit_records
		WHERE (typeof(request_body) = 'text' AND request_body <> '')
		   OR (typeof(response_body) = 'text' AND response_body <> '')`).
		Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("%d legacy text bodies remain after concurrent compact", remaining)
	}
}
