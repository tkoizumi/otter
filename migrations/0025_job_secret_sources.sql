-- Which source last satisfied each declared secret, per job.
--
-- The control plane cannot read a secret value back (CL-13): the writer is
-- write-only and the IAM policy denies GetSecretValue. So "is this job's
-- credential configured?" cannot be answered from the store — only the runtime
-- knows, because the runtime is the thing that resolves it.
--
-- This table is that answer. It is written when a run resolves its manifest's
-- secrets, records the SOURCE (`platform`, `environment`) and never a value, and
-- is read for the job view so a dashboard can say "resolved from the runtime's
-- environment" or "missing" instead of guessing. `missing` and `unavailable` are
-- recorded too: a broker outage must not look like a job with no credentials.
--
-- One row per (job, name) — the latest observation wins. It is a REPORT, not
-- configuration: deleting it changes nothing about how a run resolves secrets.

CREATE TABLE IF NOT EXISTS job_secret_sources (
    job_id      TEXT NOT NULL,
    name        TEXT NOT NULL,
    source      TEXT NOT NULL,
    run_id      TEXT NOT NULL DEFAULT '',
    observed_at DATETIME NOT NULL,
    PRIMARY KEY (job_id, name)
);
