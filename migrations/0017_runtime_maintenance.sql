-- Runtime maintenance state.
--
-- A maintenance flag is not configuration: it is a durable operational fact
-- about this runtime, and a restart must not clear it. The pooled lifecycle
-- starts a runtime in maintenance, validates it, and only then activates it;
-- if the flag lived in memory or in a config file, a crash between those steps
-- would bring the runtime back up *serving*, which is the one thing the flag
-- exists to prevent.
--
-- Single row, enforced by the CHECK on the primary key: this is a property of
-- the runtime, not of a job, so there is nowhere else for a second row to
-- belong and no reason to allow one.
--
--   mode        'startup'      set by a start that has not been activated yet
--               'draining'     operator asked; running work is finishing
--               'maintenance'  operator asked; nothing is running
--   explicit    whether an operator has made a decision about this runtime.
--               A 'startup' row is not a decision -- it is where a start
--               begins -- so it is recorded as 0 and the daemon may keep it
--               gated across restarts without claiming the operator chose it.
--   entered_at  when the current mode began, so an operator can see how long a
--               runtime has been held back rather than only that it is.
--   reason      free text from the caller, for the audit trail.
--   requested   the mode the operator asked for. Persisted so a restart can
--               distinguish "an operator chose maintenance" from "this process
--               started into it", which is what decides whether the next boot
--               resumes maintenance or merely starts gated again.

CREATE TABLE IF NOT EXISTS runtime_maintenance (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    mode        TEXT NOT NULL,
    explicit    INTEGER NOT NULL DEFAULT 0,
    entered_at  DATETIME NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    requested   TEXT NOT NULL DEFAULT ''
);
