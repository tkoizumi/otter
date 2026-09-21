-- Integration identity registry.
--
-- Identity is a UUID minted by the runtime and recorded in a `.otter-id`
-- marker inside the source directory. It is deliberately NOT the manifest
-- name: a name is a mutable, non-unique label, and a directory is a location
-- that a copy or a move changes.
--
-- `integration_instances` is the durable record of every instance that has
-- ever been registered, including retired and deleted ones, so an id is never
-- reused. `integration_paths` is the single current owner of each canonical
-- source path. `identity_operations` is the journal that makes a mutation which
-- spans SQLite and the filesystem recoverable.
--
-- This migration only creates schema. Populating the registry from a legacy
-- name-keyed workspace is a separate, gated bootstrap step: the daemon must
-- never begin scheduling merely because the SQL version advanced.

CREATE TABLE IF NOT EXISTS integration_instances (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL DEFAULT '',
    canonical_path    TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL,
    generation        INTEGER NOT NULL DEFAULT 1,
    created_at        DATETIME NOT NULL,
    updated_at        DATETIME NOT NULL,
    retired_at        DATETIME,
    retirement_reason TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_instances_status ON integration_instances (status);
CREATE INDEX IF NOT EXISTS idx_instances_path ON integration_instances (canonical_path);
CREATE INDEX IF NOT EXISTS idx_instances_name ON integration_instances (name, status);

-- One row per canonical source path. owner_id is the instance that currently
-- owns the path, or NULL when the path is unowned, retired or suppressed.
-- suppressed marks a path whose instance was deleted: discovery reports it and
-- refuses to auto-register until an explicit register clears the suppression.
CREATE TABLE IF NOT EXISTS integration_paths (
    canonical_path     TEXT PRIMARY KEY,
    owner_id           TEXT,
    suppressed         INTEGER NOT NULL DEFAULT 0,
    suppression_reason TEXT NOT NULL DEFAULT '',
    updated_at         DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_paths_owner ON integration_paths (owner_id);

CREATE TABLE IF NOT EXISTS identity_operations (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    phase       TEXT NOT NULL,
    instance_id TEXT NOT NULL DEFAULT '',
    from_path   TEXT NOT NULL DEFAULT '',
    to_path     TEXT NOT NULL DEFAULT '',
    data        TEXT NOT NULL DEFAULT '{}',
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_identity_operations_phase ON identity_operations (phase, created_at);

CREATE TABLE IF NOT EXISTS identity_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- A run records the identity generation and the label it was submitted under.
-- The generation is what fences a worker, a retry or a state write that was
-- authorized before a reset, move, retirement or deletion.
ALTER TABLE runs ADD COLUMN integration_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN integration_name TEXT NOT NULL DEFAULT '';

-- The queue carries the same generation so a claim can be refused when the
-- instance moved on between submission and execution.
ALTER TABLE run_queue ADD COLUMN integration_generation INTEGER NOT NULL DEFAULT 0;
