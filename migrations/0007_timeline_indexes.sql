-- Chronological pagination indexes for the run timeline (`otter trace`).
--
-- A timeline page merges two sources in timestamp order, and both need an
-- ordered seek rather than a full scan of the run's rows:
--
--   run_logs       ... WHERE run_id = ? AND (timestamp, id) > (?, ?) ORDER BY timestamp, id
--   http_exchanges ... WHERE run_id = ? AND (occurred_at, id) > (?, ?) ORDER BY occurred_at, id
--
-- The existing indexes are (run_id, id) on both tables. Their leading column is
-- run_id but their second is the surrogate id, which is assignment order, not
-- chronological order: log rows arrive in bursts and capture events are ingested
-- in batches, so a query ordered by timestamp would have to read and sort every
-- row for the run. These two indexes make each page a bounded index seek.
--
-- idx_run_logs_run_time is also what makes the O(1) evidence revision possible:
-- MIN(id) and MAX(id) for a run are both covering seeks on (run_id, id) and
-- (run_id, timestamp, id) rather than a count over the run's index entries.

CREATE INDEX IF NOT EXISTS idx_run_logs_run_time ON run_logs (run_id, timestamp, id);

CREATE INDEX IF NOT EXISTS idx_http_exchanges_run_time ON http_exchanges (run_id, occurred_at, id);
