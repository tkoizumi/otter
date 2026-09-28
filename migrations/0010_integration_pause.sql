-- Pausing an integration suspends its autonomous triggers -- cron and webhook
-- -- without retiring its identity, its state, its history, its tokens or its
-- releases. It is an operator control, not a lifecycle change.
--
-- A row exists only while an integration is paused, so "no row" is
-- unambiguously enabled and the common case costs nothing to read.
--
-- The key is the durable identity id, never the label or the source path: a
-- label need not be unique and a moved directory keeps its identity, so a pause
-- keyed by either would follow the wrong integration. That is what makes a move
-- carry its pause along and a reset (which mints a fresh identity) come back
-- enabled.
CREATE TABLE IF NOT EXISTS integration_pause (
    integration_id TEXT PRIMARY KEY,
    paused_at      DATETIME NOT NULL
);
