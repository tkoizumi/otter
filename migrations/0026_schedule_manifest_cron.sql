-- The last manifest cron a schedule row was seeded from.
--
-- A manifest-declared trigger is a SEED, not an owner: the row stays editable
-- from the dashboard, and a reload only applies the manifest when its cron
-- actually changed. Without this column a reload cannot tell "the user edited
-- the cadence" from "the file still says what it always said", and would either
-- clobber the edit or never apply a deliberate change.
ALTER TABLE schedules ADD COLUMN manifest_cron TEXT NOT NULL DEFAULT '';
