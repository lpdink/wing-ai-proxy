package audit

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

// compressBody returns the value to persist for an audit body. Empty strings
// are stored as empty TEXT so that `typeof(column) = 'blob'` reliably
// identifies gzip-compressed rows; everything else is gzip-compressed.
func compressBody(body string) (any, error) {
	if body == "" {
		return "", nil
	}
	compressed, err := gzipEncode([]byte(body))
	if err != nil {
		return nil, err
	}
	return compressed, nil
}

// gzipEncode compresses data with gzip.
func gzipEncode(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, fmt.Errorf("audit: gzip write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("audit: gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// gzipDecode decompresses gzip-encoded data. The store itself only writes
// bodies (operators read them back out of band with standard tools), so this
// helper is currently exercised by tests only.
func gzipDecode(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("audit: gzip reader: %w", err)
	}
	defer zr.Close()

	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("audit: gzip read: %w", err)
	}
	return out, nil
}
