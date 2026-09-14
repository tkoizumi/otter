-- Managed Python environments.
--
-- Added with the managed-Python feature so a run records exactly which
-- interpreter executed it. Empty values mean the run did not use a managed
-- environment, which is what every legacy run looks like.

ALTER TABLE runs ADD COLUMN python_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN python_version TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN environment_digest TEXT NOT NULL DEFAULT '';
-- python_policy is the preparation-policy fingerprint folded into
-- environment_digest. Recording it lets the daemon rebuild a run's environment
-- identity without invoking uv, and lets a retry keep resolving the environment
-- its parent selected.
ALTER TABLE runs ADD COLUMN python_policy TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN sdk_version TEXT NOT NULL DEFAULT '';

-- Immutable source releases.
--
-- A managed integration is served from a staged snapshot rather than its live
-- source tree. release_source_dir is the absolute directory the attempt
-- executes, recorded at submission so a retry keeps running the snapshot its
-- parent selected even after a newer release is activated. Empty means the run
-- executes the live source tree, which is what every legacy run and every
-- integration that did not opt in looks like.
ALTER TABLE runs ADD COLUMN release_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN release_source_dir TEXT NOT NULL DEFAULT '';
