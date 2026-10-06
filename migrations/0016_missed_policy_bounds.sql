-- Per-schedule missed-occurrence behaviour.
--
-- `missed_policy` has been a column since 0014, but only `skip` was
-- implemented: the other two were reserved names, refused at the write
-- boundary, so the choice could become data without a migration. This is that
-- migration's other half, and it adds the one value the policies need.
--
-- `max_catch_up` bounds how many missed occurrences one `catch_up` schedule
-- replays on a single wake-up, so a long outage cannot become a thundering
-- herd. 0 means "use the daemon default" rather than "replay nothing": a
-- default must be able to move with a release, and a stored 0 would pin every
-- existing row to whatever the default was the day it was written. The daemon
-- writes an explicit value only when an operator asked for one.
--
-- The occurrence ledger (schedule_fires, 0014) stays the single source of truth
-- for "this occurrence is done": a replayed occurrence is claimed in the same
-- transaction as the run it produces, so a cap that is raised later cannot
-- double-fire an occurrence that was already skipped.

ALTER TABLE schedules ADD COLUMN max_catch_up INTEGER NOT NULL DEFAULT 0;
