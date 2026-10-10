package audit

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// defaultCompactBatchSize is the number of rows compressed per transaction.
const defaultCompactBatchSize = 500

// CompactStats reports cumulative progress of a compaction run.
type CompactStats struct {
	RowsProcessed int64
	InputBytes    int64
	OutputBytes   int64
}

// CompactOptions configures Compact.
type CompactOptions struct {
	// BatchSize is the number of rows compressed per transaction (default 500).
	BatchSize int
	// Vacuum runs VACUUM after compaction and reports the reclaimed bytes.
	Vacuum bool
	// Progress, if set, is called after each committed batch with cumulative stats.
	Progress func(CompactStats)
}

// CompactResult is returned by Compact.
type CompactResult struct {
	CompactStats
	// ReclaimedBytes is the disk space released by VACUUM (0 unless Vacuum is set).
	ReclaimedBytes int64
}

// compactDSN builds the DSN for the compact command. Batch transactions must
// begin with BEGIN IMMEDIATE: upgrading a deferred read transaction to a write
// after a concurrent writer has committed fails with SQLITE_BUSY_SNAPSHOT,
// which the busy timeout cannot resolve.
func compactDSN(dsn string) string {
	return sqliteDSN(dsn) + "&_txlock=immediate"
}

// Compact compresses legacy plain-TEXT request/response bodies in the audit
// database in place. Rows already holding compressed BLOBs and empty strings
// are left untouched, so the command is safe to run (and re-run) while a proxy
// instance is writing new rows to the same database.
func Compact(dsn string, opts CompactOptions) (CompactResult, error) {
	var result CompactResult
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultCompactBatchSize
	}

	// A mistyped path must fail instead of silently creating an empty database.
	if dsn != ":memory:" && !strings.HasPrefix(dsn, "file:") {
		if _, err := os.Stat(dsn); err != nil {
			return result, fmt.Errorf("audit: compact: %w", err)
		}
	}

	db, err := sql.Open("sqlite", compactDSN(dsn))
	if err != nil {
		return result, fmt.Errorf("audit: compact: open sqlite: %w", err)
	}
	defer db.Close()

	var tableName string
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'audit_records'").Scan(&tableName)
	if errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("audit: compact: table audit_records not found in %s", dsn)
	}
	if err != nil {
		return result, fmt.Errorf("audit: compact: inspect database: %w", err)
	}

	// Snapshot the current max id so the loop terminates even if an old proxy
	// version keeps appending uncompressed rows while we run. Rows appended
	// later are picked up by a subsequent compact run.
	var maxID int64
	if err := db.QueryRow("SELECT COALESCE(MAX(id), 0) FROM audit_records").Scan(&maxID); err != nil {
		return result, fmt.Errorf("audit: compact: max id: %w", err)
	}

	for {
		ids, err := selectCompactBatch(db, maxID, batchSize)
		if err != nil {
			return result, err
		}
		if len(ids) == 0 {
			break
		}

		tx, err := db.Begin()
		if err != nil {
			return result, fmt.Errorf("audit: compact: begin: %w", err)
		}

		batchStats := CompactStats{}
		for _, id := range ids {
			delta, err := compactRow(tx, id)
			if err != nil {
				tx.Rollback()
				return result, err
			}
			batchStats.RowsProcessed += delta.RowsProcessed
			batchStats.InputBytes += delta.InputBytes
			batchStats.OutputBytes += delta.OutputBytes
		}

		if err := tx.Commit(); err != nil {
			return result, fmt.Errorf("audit: compact: commit: %w", err)
		}

		result.RowsProcessed += batchStats.RowsProcessed
		result.InputBytes += batchStats.InputBytes
		result.OutputBytes += batchStats.OutputBytes
		if opts.Progress != nil {
			opts.Progress(result.CompactStats)
		}
	}

	if opts.Vacuum {
		reclaimed, err := vacuum(db, dsn)
		if err != nil {
			return result, fmt.Errorf("audit: compact: vacuum: %w", err)
		}
		result.ReclaimedBytes = reclaimed
	}

	return result, nil
}

// selectCompactBatch returns the ids of the next batch of rows that still hold
// legacy plain-TEXT bodies. Processed rows no longer match the typeof()
// predicates, so the same query can be repeated without an OFFSET.
func selectCompactBatch(db *sql.DB, maxID int64, limit int) ([]int64, error) {
	const query = `
		SELECT id FROM audit_records
		WHERE id <= ?
		  AND ((typeof(request_body) = 'text' AND request_body <> '')
		    OR (typeof(response_body) = 'text' AND response_body <> ''))
		ORDER BY id
		LIMIT ?`

	rows, err := db.Query(query, maxID, limit)
	if err != nil {
		return nil, fmt.Errorf("audit: compact: select batch: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("audit: compact: scan batch id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: compact: iterate batch: %w", err)
	}
	return ids, nil
}

// compactRow compresses the legacy bodies of a single row inside tx and
// returns the byte counters for it.
func compactRow(tx *sql.Tx, id int64) (CompactStats, error) {
	var stats CompactStats
	var reqBody, respBody []byte
	var reqType, respType string

	const query = `
		SELECT request_body, response_body, typeof(request_body), typeof(response_body)
		FROM audit_records WHERE id = ?`
	err := tx.QueryRow(query, id).Scan(&reqBody, &respBody, &reqType, &respType)
	if errors.Is(err, sql.ErrNoRows) {
		return stats, nil // row deleted concurrently; nothing to do
	}
	if err != nil {
		return stats, fmt.Errorf("audit: compact: read row %d: %w", id, err)
	}

	newReq, inReq, outReq, err := compressLegacyBody(reqBody, reqType)
	if err != nil {
		return stats, fmt.Errorf("audit: compact: row %d request_body: %w", id, err)
	}
	newResp, inResp, outResp, err := compressLegacyBody(respBody, respType)
	if err != nil {
		return stats, fmt.Errorf("audit: compact: row %d response_body: %w", id, err)
	}
	stats.InputBytes = inReq + inResp
	stats.OutputBytes = outReq + outResp

	switch {
	case newReq != nil && newResp != nil:
		_, err = tx.Exec("UPDATE audit_records SET request_body = ?, response_body = ? WHERE id = ?", newReq, newResp, id)
	case newReq != nil:
		_, err = tx.Exec("UPDATE audit_records SET request_body = ? WHERE id = ?", newReq, id)
	case newResp != nil:
		_, err = tx.Exec("UPDATE audit_records SET response_body = ? WHERE id = ?", newResp, id)
	default:
		return stats, nil // nothing to compress
	}
	if err != nil {
		return stats, fmt.Errorf("audit: compact: update row %d: %w", id, err)
	}
	stats.RowsProcessed = 1
	return stats, nil
}

// compressLegacyBody returns the gzip-compressed body for a legacy plain-TEXT
// column, together with the input/output byte counts. It returns nil when the
// column needs no conversion: compressed BLOBs and empty strings are left
// as they are.
func compressLegacyBody(body []byte, bodyType string) (compressed []byte, inputBytes, outputBytes int64, err error) {
	if bodyType != "text" || len(body) == 0 {
		return nil, 0, 0, nil
	}
	compressed, err = gzipEncode(body)
	if err != nil {
		return nil, 0, 0, err
	}
	return compressed, int64(len(body)), int64(len(compressed)), nil
}

// vacuum checkpoints the WAL and runs VACUUM, returning the number of bytes
// reclaimed in the main database file.
func vacuum(db *sql.DB, dsn string) (int64, error) {
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return 0, fmt.Errorf("wal checkpoint: %w", err)
	}
	before := fileSize(dsn)
	if _, err := db.Exec("VACUUM"); err != nil {
		return 0, err
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return 0, fmt.Errorf("wal checkpoint after vacuum: %w", err)
	}
	return before - fileSize(dsn), nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
