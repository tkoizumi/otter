-- Schedules are runtime state, not manifest state.
--
-- A job's cadence changes far more often than its code: "every 15 minutes"
-- becomes "hourly" because of load, cost or a vendor's rate limit. Carrying it
-- in otter.yaml would mean a release per cadence change, and -- worse -- a
-- declarative value that a runtime change silently contradicts. That is the
-- same trap job_pause exists to avoid.
--
-- One row per job. "No row" means the job has never been given a schedule,
-- which is what lets a manifest's trigger.cron seed it once during the
-- deprecation window. A row with an empty cron means the operator cleared the
-- schedule explicitly, so a reload must not seed it back.
CREATE TABLE IF NOT EXISTS job_schedules (
    job_id     TEXT PRIMARY KEY,
    cron       TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
