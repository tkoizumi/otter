-- The local outbox for capture export.
--
-- A completed exchange, or a run's manifest, is recorded here in the SAME
-- transaction that stores it. That is what makes the promise "an exchange that
-- exists locally is eventually exported" true without making the sink part of
-- the ingest path: the row and the exchange commit together, and the relay
-- (S4) drains the row later.
--
-- One row per exchange and one per run manifest. `kind` is in the primary key
-- because a manifest has no request id and because a replay (R1) is a separate
-- document from the exchange it replays. `state` is pending, sent or abandoned;
-- `attempts`, `last_error` and `next_attempt_at` are the relay's retry state.
-- A failed export never fails ingest: this table is diagnostic work, not the
-- job's outcome.
CREATE TABLE IF NOT EXISTS capture_exports (
    run_id          TEXT NOT NULL,
    kind            TEXT NOT NULL,
    request_id      TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL DEFAULT 'pending',
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (run_id, kind, request_id)
);

-- The relay asks for pending rows whose backoff has elapsed, oldest first.
CREATE INDEX IF NOT EXISTS idx_capture_exports_pending
    ON capture_exports (state, next_attempt_at, created_at);
