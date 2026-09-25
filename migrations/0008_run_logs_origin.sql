-- Record who wrote each log line, so the timeline can tell the daemon's own
-- narration apart from an integration's ctx.log output.
--
-- Three writers share the `otter` stream: the daemon's lifecycle narration
-- (appendOtterLog), the SDK's structured logger (POST /logs with stream=otter),
-- and captured child stdout/stderr. Classifying by stream alone therefore
-- labelled every ctx.log line a "lifecycle" event, which is wrong: it is the
-- integration's own output, and the two need different treatment when read.
--
-- The origin is recorded at write time rather than inferred at read time. The
-- fallback below still exists for rows written before this column, but new rows
-- never depend on parsing a message to know where they came from.

ALTER TABLE run_logs ADD COLUMN origin TEXT NOT NULL DEFAULT '';

-- Backfill by message family: the daemon's narration is a small, fixed set of
-- shapes emitted by internal/daemon (see the appendOtterLog call sites), and
-- runs.LooksLikeDaemonNarration is the same rule in Go. Keep the two in step: a
-- test pins the Go rule against the real strings, and these patterns mirror it.
--
-- The patterns require the status word to be punctuated, which is what every
-- narration line does and what a child's own sentence like "run failed because
-- X" does not. This is a one-time best effort for rows written before the column
-- existed; everything unrecognised on the otter stream was written by the SDK's
-- logger, and everything on stdout/stderr by the child.
UPDATE run_logs SET origin = 'daemon' WHERE origin = '' AND (
       message LIKE 'run queued (%'
    OR message LIKE 'run started (%'
    OR message LIKE 'run cancelled before execution%'
    OR message LIKE 'run cancelled:%'
    OR message LIKE 'run failed (%'
    OR message LIKE 'run failed:%'
    OR message LIKE 'run failed,%'
    OR message LIKE 'run succeeded (%'
    OR message LIKE 'run succeeded:%'
    OR message LIKE 'run succeeded,%'
    OR message LIKE 'run timed_out (%'
    OR message LIKE 'run timed out (%'
    OR message LIKE 'run retrying (%'
    OR message LIKE 'retry % scheduled in %'
    OR message LIKE 'not started: %'
    OR message LIKE 'marked failed: %'
);

UPDATE run_logs SET origin = 'child' WHERE origin = '';
