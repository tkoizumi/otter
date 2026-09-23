-- Bounded HTTP request inspection.
--
-- Two tables. `run_capture` is the small per-run summary that survives payload
-- retention; `http_exchanges` is the per-request record.
--
-- The absence of a `run_capture` row is meaningful: a run without one predates
-- capture (or was submitted with capture off before capture existed) and must be
-- reported as "unavailable", never as a recording that observed zero requests.
-- That is why the row is written when the run is submitted rather than when the
-- first event arrives.
--
-- Bodies are stored as already-sanitized JSON. Every row is bounded by the
-- limits in internal/inspection, and the summary carries the dropped and
-- redaction counters so a truncated recording is never mistaken for a complete
-- one.

CREATE TABLE IF NOT EXISTS run_capture (
    run_id           TEXT PRIMARY KEY,
    integration_id   TEXT NOT NULL DEFAULT '',
    policy           TEXT NOT NULL,
    schema_version   INTEGER NOT NULL,
    policy_version   INTEGER NOT NULL,
    adapters         TEXT NOT NULL DEFAULT '[]',
    coverage         TEXT NOT NULL DEFAULT '',
    started_at       DATETIME NOT NULL,
    finalized_at     DATETIME,
    -- pending | complete | incomplete | abrupt
    finalization     TEXT NOT NULL DEFAULT 'pending',
    request_count    INTEGER NOT NULL DEFAULT 0,
    completed_count  INTEGER NOT NULL DEFAULT 0,
    failed_count     INTEGER NOT NULL DEFAULT 0,
    incomplete_count INTEGER NOT NULL DEFAULT 0,
    dropped_events   INTEGER NOT NULL DEFAULT 0,
    dropped_bytes    INTEGER NOT NULL DEFAULT 0,
    stored_bytes     INTEGER NOT NULL DEFAULT 0,
    redaction_count  INTEGER NOT NULL DEFAULT 0,
    last_sequence    INTEGER NOT NULL DEFAULT 0,
    -- Set when retention removed the requests but kept this summary, so an
    -- expired recording is distinguishable from one that observed nothing.
    payloads_expired INTEGER NOT NULL DEFAULT 0,
    expired_at       DATETIME,
    note             TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_run_capture_finalization ON run_capture (finalization, started_at);

CREATE TABLE IF NOT EXISTS http_exchanges (
    -- id is the ingestion cursor: it is assigned by this database, is monotonic,
    -- and is what pagination and the timeline order by. producer_seq and
    -- request_id are supplied by the SDK and carry execution relationships only.
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id         TEXT NOT NULL,
    request_id     TEXT NOT NULL,
    integration_id TEXT NOT NULL DEFAULT '',
    producer_seq   INTEGER NOT NULL DEFAULT 0,
    occurred_at    DATETIME NOT NULL,
    ingested_at    DATETIME NOT NULL,
    updated_at     DATETIME NOT NULL,
    -- in_progress | completed | failed
    phase          TEXT NOT NULL,
    complete       INTEGER NOT NULL DEFAULT 0,
    method         TEXT NOT NULL DEFAULT '',
    sanitized_url  TEXT NOT NULL DEFAULT '',
    -- initial_url/final_url expose a redirect chain without claiming to have
    -- captured each internal hop as a separate request.
    initial_url    TEXT NOT NULL DEFAULT '',
    final_url      TEXT NOT NULL DEFAULT '',
    call_site      TEXT NOT NULL DEFAULT '',
    status_code    INTEGER,
    transport_error       TEXT NOT NULL DEFAULT '',
    transport_error_class TEXT NOT NULL DEFAULT '',
    duration_to_headers_ms INTEGER,
    duration_body_ms       INTEGER,
    duration_total_ms      INTEGER,
    -- Ordered [{"name":..,"value":..}] arrays, so duplicate header names and
    -- their original order survive.
    request_headers  TEXT NOT NULL DEFAULT '[]',
    response_headers TEXT NOT NULL DEFAULT '[]',
    -- Sanitized body descriptors, or '' when the event never mentioned a body.
    request_body  TEXT NOT NULL DEFAULT '',
    response_body TEXT NOT NULL DEFAULT '',
    -- Body coverage for this exchange, maintained at write time so the list view
    -- can report completeness without loading any payload: metadata | partial |
    -- full. It is 'metadata' whenever the run's policy captured no payloads, so
    -- the list never implies that a body was inspected when it was not.
    payloads TEXT NOT NULL DEFAULT 'metadata',
    UNIQUE (run_id, request_id)
);

CREATE INDEX IF NOT EXISTS idx_http_exchanges_run ON http_exchanges (run_id, id);
CREATE INDEX IF NOT EXISTS idx_http_exchanges_ingested ON http_exchanges (ingested_at);
