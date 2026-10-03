-- Job configuration: the third layer between the release and the run.
--
-- `otter.yaml` `env` is part of the release, which is correct -- a manifest is
-- code, snapshotted per run. But deployment values (a store name, a dataset id,
-- a backfill date) are not code, and carrying them in the manifest means a
-- release per tenant and no audit record for a change.
--
-- The naive fix -- read configuration live at execution -- would break the
-- guarantee that a retry executes what was accepted. So configuration is
-- versioned and immutable, and a run pins the version it resolved at
-- submission: a queued, retrying or backlogged run keeps reading the values it
-- was accepted with, exactly as it keeps its release.
--
-- Runtime-owned: a runtime must execute without the cloud, and accepted work is
-- authoritative on the runtime (CL-05). The control plane writes versions
-- through the API and reads them for display; it is not the store.

CREATE TABLE IF NOT EXISTS job_configs (
    id         TEXT PRIMARY KEY,          -- immutable version id
    job_id     TEXT NOT NULL,             -- durable identity, never a label
    config_values TEXT NOT NULL,          -- JSON object
    created_at DATETIME NOT NULL,
    created_by TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_job_configs_job ON job_configs (job_id, created_at DESC);

-- The mutable pointer: which version a new run pins. Absence means the job has
-- no configuration, which is the common case.
CREATE TABLE IF NOT EXISTS job_config_current (
    job_id     TEXT PRIMARY KEY,
    config_id  TEXT NOT NULL,
    updated_at DATETIME NOT NULL
);

-- Which configuration version a run resolved at submission. Empty/NULL means
-- the job had none when the run was accepted.
ALTER TABLE runs ADD COLUMN config_version TEXT;
