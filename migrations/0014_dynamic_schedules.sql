-- Dynamic schedules.
--
-- v0.3.0 shipped a single cadence per job in job_schedules. A control plane
-- needs a first-class schedule: several per job, each with its own payload, time
-- zone and pause state, each addressable by id. This migration replaces the
-- one-row-per-job table with that model.
--
-- Two columns carry the design:
--
--   origin     what owns the row. A manifest-declared schedule reconciles on
--              reload; an API-created one is never read, changed or deleted by a
--              reload. Without it a declarative file and an imperative control
--              plane silently undo each other.
--   paused_at  absence means active, mirroring job_pause, so the two pause
--              levels read the same way.
--
-- The occurrence ledger (schedule_fires) is written in the same transaction as
-- the run it produced, so a crash between firing and recording is impossible and
-- a duplicate wake-up loses to the primary key instead of running twice.
CREATE TABLE IF NOT EXISTS schedules (
    id              TEXT PRIMARY KEY,
    job_id          TEXT NOT NULL,
    cron            TEXT NOT NULL,
    timezone        TEXT NOT NULL DEFAULT 'UTC',
    payload         TEXT NOT NULL DEFAULT '{}',
    config_version  TEXT,
    missed_policy   TEXT NOT NULL DEFAULT 'skip',
    origin          TEXT NOT NULL,
    origin_ref      TEXT NOT NULL DEFAULT '',
    paused_at       DATETIME,
    last_fired_at   DATETIME,
    idempotency_key TEXT,
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_schedules_job ON schedules (job_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_schedules_idempotency
    ON schedules (idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS schedule_fires (
    schedule_id   TEXT NOT NULL,
    occurrence_at DATETIME NOT NULL,
    run_id        TEXT NOT NULL,
    fired_at      DATETIME NOT NULL,
    PRIMARY KEY (schedule_id, occurrence_at)
);

ALTER TABLE runs ADD COLUMN schedule_id TEXT;

CREATE INDEX IF NOT EXISTS idx_runs_schedule ON runs (schedule_id, created_at DESC);

-- Carry forward every v0.3.0 cadence.
--
-- The old table did not record an owner, so a carried row is stored as
-- origin = 'api' and never reconciled. That preserves exactly what the old
-- "seed once" rule promised: a reload could never overwrite or delete a cadence
-- an operator had set, whether it came from a manifest seed or PUT. New
-- manifest-declared schedules for jobs that have no row are reconciled as
-- origin = 'manifest' from here on.
INSERT INTO schedules (
    id, job_id, cron, timezone, payload, missed_policy, origin, origin_ref,
    created_at, updated_at)
SELECT
    lower(hex(randomblob(16))), job_id, cron, 'UTC', '{}', 'skip', 'api',
    'migrated-v0.3.0', created_at, updated_at
FROM job_schedules;

DROP TABLE IF EXISTS job_schedules;
