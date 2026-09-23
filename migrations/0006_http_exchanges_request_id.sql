-- Lookup path for inspecting one request without naming its run.
--
-- `otter request <request-id>` resolves the owning run from the stored row, so
-- the read is by request_id alone. The existing UNIQUE (run_id, request_id)
-- index cannot serve it: its leading column is run_id.
--
-- This index is deliberately not a uniqueness guarantee. request_id is
-- SDK-supplied metadata, and the UNIQUE constraint above only promises per-run
-- uniqueness; two runs may hold the same id. A reader that matches more than one
-- row is told the id is ambiguous rather than handed an arbitrary one.

CREATE INDEX IF NOT EXISTS idx_http_exchanges_request_id ON http_exchanges (request_id);
