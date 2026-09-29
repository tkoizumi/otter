-- Rename the job model without changing identities, data, or execution history.
-- Earlier migrations are immutable upgrade history and retain their old names.
ALTER TABLE integration_state RENAME TO job_state;
ALTER TABLE job_state RENAME COLUMN integration_id TO job_id;
ALTER TABLE integration_instances RENAME TO job_instances;
ALTER TABLE integration_paths RENAME TO job_paths;
ALTER TABLE integration_pause RENAME TO job_pause;
ALTER TABLE job_pause RENAME COLUMN integration_id TO job_id;
ALTER TABLE runs RENAME COLUMN integration_id TO job_id;
ALTER TABLE runs RENAME COLUMN integration_generation TO job_generation;
ALTER TABLE runs RENAME COLUMN integration_name TO job_name;
ALTER TABLE run_queue RENAME COLUMN integration_id TO job_id;
ALTER TABLE run_queue RENAME COLUMN integration_generation TO job_generation;
ALTER TABLE webhook_tokens RENAME COLUMN integration_id TO job_id;
ALTER TABLE run_capture RENAME COLUMN integration_id TO job_id;
ALTER TABLE http_exchanges RENAME COLUMN integration_id TO job_id;
DROP INDEX idx_runs_integration;
CREATE INDEX idx_runs_job ON runs (job_id, created_at DESC);
