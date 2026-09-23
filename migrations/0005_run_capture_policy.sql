-- The HTTP capture policy a run was submitted with.
--
-- It lives on the run rather than only in the capture summary because a retry
-- must inherit exactly what its parent was submitted with: the policy is a
-- property of the request an operator made, not of the attempt that happens to
-- be executing. The per-run summary in run_capture remains the place a reader
-- looks to learn whether anything was actually recorded.
--
-- The default is the empty string, which means "predates capture". New runs
-- always record an explicit policy, so an empty value is unambiguous.

ALTER TABLE runs ADD COLUMN capture_policy TEXT NOT NULL DEFAULT '';
