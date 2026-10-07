-- Runtime-side idempotency for control commands.
--
-- The agent-mediated control channel is at-least-once: Cloud may deliver a
-- command again after a lost acknowledgement, and the agent may retry a call
-- whose response never arrived. "Run job" must therefore produce exactly ONE
-- run, and the deduplication has to happen at the runtime -- the component that
-- actually accepts work -- because Cloud's copy of the request can be lost with
-- the isolate that held it.
--
-- The key is caller-derived (an Idempotency-Key on the submit request), not the
-- run id and not a timestamp: a key the runtime invented would be different on
-- every delivery, which is exactly the duplication this table exists to stop.
--
--   job_id           the key is scoped to one job, so two jobs can use the same
--                    caller key without colliding;
--   run_id           the run the first delivery produced; a later delivery
--                    returns this id instead of creating a second run.
--
-- The row is written in the SAME transaction as the run and its queue entry, so
-- a crash cannot leave a key pointing at a run that does not exist.
CREATE TABLE run_idempotency (
    job_id          TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    run_id          TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    PRIMARY KEY (job_id, idempotency_key)
);

-- A run is reachable from the key that produced it, for diagnostics and for a
-- cleanup pass that prunes keys once their run is terminal and old.
CREATE INDEX idx_run_idempotency_run ON run_idempotency(run_id);
