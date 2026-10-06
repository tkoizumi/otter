-- Operation state for the pooled pilot.
--
-- Design: otter-platform/hosting/docs/operations-model.md. This is the pilot's
-- slice of it -- one runtime, one host, one operator -- and it is deliberately
-- the smallest schema that makes the three properties true:
--
--   1. intent is durable before any side effect;
--   2. no step is applied twice;
--   3. an unknown outcome is never resolved by guessing.
--
-- The column that earns its place is `result = 'unknown'`. A deploy that lost
-- contact mid-swap is not 'failed' (the change may have applied) and not
-- 'applied' (it may not have), and collapsing it into either is how a runtime
-- ends up in a state nobody chose. It is a stored value rather than an inferred
-- one for exactly that reason.
--
--   step    where the operation got to. Free text rather than an enum, because
--           the apply sequence is the agent's business and a schema that
--           enumerated it would need a migration every time the protocol moves.
--   result  'pending' | 'applied' | 'failed' | 'abandoned' | 'unknown'
--   fencing evidence used to decide an unknown outcome: the last observed
--           generation and lease, so an operator can tell "the agent is gone"
--           from "the agent is on an older generation".
--
-- `observed` is filled from agent reports and NEVER from a delivered command:
-- under a pull model no command is delivered, and inferring execution from
-- delivery is the mistake the pull design exists to avoid.
CREATE TABLE operations (
    id                  TEXT PRIMARY KEY,
    runtime_id          TEXT NOT NULL,
    -- The generation the operation was planned against, so a concurrent change
    -- is detected rather than silently overwritten.
    expected_generation INTEGER NOT NULL,
    -- What the operation wants to be true when it finishes.
    desired_digest      TEXT NOT NULL DEFAULT '',
    desired_config_json TEXT NOT NULL DEFAULT '{}',
    -- What the runtime last REPORTED. Empty means nothing has been heard, which
    -- is a different fact from "the runtime is on generation 0".
    observed_generation INTEGER,
    observed_digest     TEXT NOT NULL DEFAULT '',
    step                TEXT NOT NULL DEFAULT '',
    result              TEXT NOT NULL DEFAULT 'pending'
        CHECK (result IN ('pending','applied','failed','abandoned','unknown')),
    reason              TEXT NOT NULL DEFAULT '',
    operator            TEXT NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);

-- One operation per runtime may be open. A partial unique index rather than a
-- lock table: the invariant is "at most one in-flight operation per runtime",
-- and expressing it in the schema means it cannot be forgotten by a caller.
CREATE UNIQUE INDEX operations_one_open_per_runtime
    ON operations (runtime_id)
    WHERE result IN ('pending','unknown');

CREATE INDEX operations_by_runtime ON operations (runtime_id, created_at DESC);

-- Runtime locks.
--
-- The model is explicit that a lock is not an authority: it protects local
-- execution and does NOT authorise takeover after a partition. A lock held by a
-- controller that cannot reach the runtime prevents a SECOND controller from
-- acting, which is the correct outcome -- the alternative is two controllers
-- racing on a stateful singleton.
--
--   expires_at  recorded but not authoritative. A lock past its expiry is
--               reported as stale for a human to clear; it is not taken over
--               automatically, because an automatic expiry could clear a live
--               lock during exactly the partition it exists to survive.
CREATE TABLE runtime_locks (
    runtime_id  TEXT PRIMARY KEY,
    holder      TEXT NOT NULL,
    operation_id TEXT NOT NULL DEFAULT '',
    acquired_at INTEGER NOT NULL,
    expires_at  INTEGER,
    reason      TEXT NOT NULL DEFAULT ''
);
