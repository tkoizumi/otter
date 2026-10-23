-- One replay is its own document.
--
-- A replay resends a captured request and records what came back. It is not a
-- second version of the original exchange: the original stays byte-identical,
-- and this row carries the template (headers and body with their
-- {{secret:NAME}} placeholders) plus the resolved-bytes digest. The resolved
-- bytes themselves never land here; only the runtime held them while sending.
--
-- `request_id` names the exchange this replays. `replay_id` is the document
-- identity the sink stores under `capture/<runtime>/<run>/replays/<id>.json`.
--
-- The row is bounded like an exchange: one body each side, capped before it is
-- written. Replays are counted against the same seven-day retention as the
-- exchange they resend, because a replay cannot outlive the template it needs.
CREATE TABLE IF NOT EXISTS http_replays (
    replay_id        TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL,
    request_id       TEXT NOT NULL,
    job_id           TEXT NOT NULL DEFAULT '',
    occurred_at      DATETIME NOT NULL,
    complete         INTEGER NOT NULL DEFAULT 1,
    method           TEXT NOT NULL DEFAULT '',
    url              TEXT NOT NULL DEFAULT '',
    request_headers  TEXT NOT NULL DEFAULT '[]',
    request_body     TEXT NOT NULL DEFAULT '',
    status_code      INTEGER,
    response_body    TEXT NOT NULL DEFAULT '',
    transport_error  TEXT NOT NULL DEFAULT '',
    transport_error_class TEXT NOT NULL DEFAULT '',
    duration_total_ms INTEGER,
    resolved_sha256  TEXT NOT NULL DEFAULT '',
    created_at       DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_http_replays_run ON http_replays (run_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_http_replays_original ON http_replays (run_id, request_id);
