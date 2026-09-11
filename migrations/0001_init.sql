-- Otter MVP schema.
--
-- All timestamps are stored as fixed-width UTC strings using the layout
--   2006-01-02T15:04:05.000000000Z07:00
-- so that lexicographic ordering matches chronological ordering and SQL
-- comparisons such as `available_at <= ?` behave correctly.

-- Durable key/value state, namespaced per integration. Values are arbitrary
-- JSON documents stored as text.
CREATE TABLE IF NOT EXISTS integration_state (
    integration_id TEXT NOT NULL,
    key            TEXT NOT NULL,
    value          TEXT,
    updated_at     DATETIME NOT NULL,
    PRIMARY KEY (integration_id, key)
);

-- One row per execution attempt. A retry creates a new row whose
-- parent_run_id points at the attempt it retries and whose attempt value is
-- incremented.
CREATE TABLE IF NOT EXISTS runs (
    id             TEXT PRIMARY KEY,
    integration_id TEXT NOT NULL,
    trigger_type   TEXT NOT NULL,
    status         TEXT NOT NULL,
    attempt        INTEGER NOT NULL DEFAULT 1,
    parent_run_id  TEXT,
    created_at     DATETIME NOT NULL,
    started_at     DATETIME,
    finished_at    DATETIME,
    exit_code      INTEGER,
    error          TEXT,
    metadata       TEXT
);

CREATE INDEX IF NOT EXISTS idx_runs_integration ON runs (integration_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs (status);
CREATE INDEX IF NOT EXISTS idx_runs_parent ON runs (parent_run_id);
CREATE INDEX IF NOT EXISTS idx_runs_created ON runs (created_at DESC);

-- Captured output from an integration process plus runtime lifecycle events.
CREATE TABLE IF NOT EXISTS run_logs (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id    TEXT NOT NULL,
    timestamp DATETIME NOT NULL,
    stream    TEXT NOT NULL,
    message   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_run_logs_run ON run_logs (run_id, id);

-- Durable run queue. A row exists here while a run is waiting to be claimed by
-- a worker; available_at implements retry backoff.
CREATE TABLE IF NOT EXISTS run_queue (
    run_id         TEXT PRIMARY KEY,
    integration_id TEXT NOT NULL,
    available_at   DATETIME NOT NULL,
    created_at     DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_run_queue_available ON run_queue (available_at, created_at);

-- Static per-integration webhook tokens. Persisted so that webhook URLs keep
-- working across daemon restarts.
CREATE TABLE IF NOT EXISTS webhook_tokens (
    integration_id TEXT PRIMARY KEY,
    token          TEXT NOT NULL,
    created_at     DATETIME NOT NULL
);
