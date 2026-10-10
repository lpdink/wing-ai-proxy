package audit

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestGzipRoundTrip(t *testing.T) {
	cases := [][]byte{
		[]byte("hello world"),
		[]byte(strings.Repeat("compress me ", 1000)),
		[]byte("中文内容 ✓ mixed with ASCII"),
		bytes.Repeat([]byte{0x00, 0xff, 0x10}, 512), // binary payload
	}

	for _, want := range cases {
		compressed, err := gzipEncode(want)
		if err != nil {
			t.Fatalf("gzipEncode: %v", err)
		}
		got, err := gzipDecode(compressed)
		if err != nil {
			t.Fatalf("gzipDecode: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("round trip mismatch: got %d bytes, want %d bytes", len(got), len(want))
		}
	}
}

func TestSQLiteStore_BodyCompression(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"` + strings.Repeat("hello ", 200) + `"}]}`
	respBody := `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`

	insertRecord := func(id, req, resp string) {
		t.Helper()
		rec := Record{
			RequestID:     id,
			VirtualAPIKey: "sk-test",
			ProviderName:  "prov",
			ModelShowName: "m",
			ModelRealName: "m-real",
			RequestStart:  time.Now(),
			FirstByteAt:   time.Now(),
			RequestEnd:    time.Now(),
			RequestBody:   req,
			ResponseBody:  resp,
		}
		if err := store.Insert(rec); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	insertRecord("cmp-full", reqBody, respBody)
	insertRecord("cmp-empty", "", "")

	// Non-empty bodies must be stored as gzip BLOBs that decode to the original.
	var reqType, respType string
	var reqRaw, respRaw []byte
	err = store.db.QueryRow(`
		SELECT typeof(request_body), request_body, typeof(response_body), response_body
		FROM audit_records WHERE request_id = 'cmp-full'`).
		Scan(&reqType, &reqRaw, &respType, &respRaw)
	if err != nil {
		t.Fatal(err)
	}
	if reqType != "blob" {
		t.Errorf("typeof(request_body) = %q, want %q", reqType, "blob")
	}
	if respType != "blob" {
		t.Errorf("typeof(response_body) = %q, want %q", respType, "blob")
	}

	gotReq, err := gzipDecode(reqRaw)
	if err != nil {
		t.Fatalf("decode request_body: %v", err)
	}
	if string(gotReq) != reqBody {
		t.Errorf("request_body round trip mismatch: got %d bytes, want %d bytes", len(gotReq), len(reqBody))
	}
	gotResp, err := gzipDecode(respRaw)
	if err != nil {
		t.Fatalf("decode response_body: %v", err)
	}
	if string(gotResp) != respBody {
		t.Errorf("response_body round trip mismatch")
	}
	if len(reqRaw) >= len(reqBody) {
		t.Errorf("compressed request_body (%d bytes) not smaller than original (%d bytes)", len(reqRaw), len(reqBody))
	}

	// Empty bodies must stay empty TEXT (so typeof() reliably distinguishes them).
	err = store.db.QueryRow(`
		SELECT typeof(request_body), request_body, typeof(response_body), response_body
		FROM audit_records WHERE request_id = 'cmp-empty'`).
		Scan(&reqType, &reqRaw, &respType, &respRaw)
	if err != nil {
		t.Fatal(err)
	}
	if reqType != "text" || respType != "text" {
		t.Errorf("empty body types = %q/%q, want text/text", reqType, respType)
	}
	if len(reqRaw) != 0 || len(respRaw) != 0 {
		t.Errorf("empty bodies not empty: %d/%d bytes", len(reqRaw), len(respRaw))
	}
}
