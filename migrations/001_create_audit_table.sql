CREATE TABLE IF NOT EXISTS audit_records (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id      TEXT    NOT NULL UNIQUE,
    virtual_api_key TEXT    NOT NULL,
    provider_name   TEXT    NOT NULL,
    model_show_name TEXT    NOT NULL,
    model_real_name TEXT    NOT NULL,
    request_start   TEXT    NOT NULL,
    first_byte_at   TEXT    NOT NULL,
    request_end     TEXT    NOT NULL,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_hit_tokens INTEGER NOT NULL DEFAULT 0,
    tool_calls      TEXT    NOT NULL DEFAULT '[]',
    is_stream       INTEGER NOT NULL DEFAULT 0,
    request_body    TEXT    NOT NULL DEFAULT '',
    response_body   TEXT    NOT NULL DEFAULT '',
    truncated       INTEGER NOT NULL DEFAULT 0,
    status_code     INTEGER NOT NULL DEFAULT 0,
    error_message   TEXT    NOT NULL DEFAULT ''
);

-- Query optimization indexes
CREATE INDEX IF NOT EXISTS idx_audit_virtual_api_key ON audit_records(virtual_api_key);
CREATE INDEX IF NOT EXISTS idx_audit_provider_name ON audit_records(provider_name);
CREATE INDEX IF NOT EXISTS idx_audit_model_show_name ON audit_records(model_show_name);
CREATE INDEX IF NOT EXISTS idx_audit_request_start ON audit_records(request_start);
CREATE INDEX IF NOT EXISTS idx_audit_composite ON audit_records(virtual_api_key, provider_name, request_start);
