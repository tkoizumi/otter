-- Scoped API tokens: operator credentials narrower than the admin token.
--
-- The admin token (OTTER_API_TOKEN) is all-or-nothing. It reads business state,
-- reads capture payloads, registers and deletes jobs, and executes arbitrary
-- code through POST /v1/jobs/{id}/runs. A gateway that commands a runtime on a
-- customer's behalf needs the command surface and nothing else, so that a
-- compromise of the gateway does not become a compromise of the tenant
-- (CL-21).
--
-- Only the SHA-256 of a token is stored. The plaintext is shown once, at
-- creation, and cannot be recovered afterwards, so a leaked database does not
-- hand over a working credential.
--
-- Revocation is a timestamp rather than a delete: the row survives for audit,
-- and every lookup filters on revoked_at IS NULL, so revocation takes effect on
-- the next request with no restart and no cache to invalidate.
--
-- The admin token is deliberately not representable here. It stays the static
-- OTTER_API_TOKEN, which the daemon reads from its environment, so a database
-- compromise cannot mint full authority.
CREATE TABLE IF NOT EXISTS api_tokens (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    scope      TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created_at DATETIME NOT NULL,
    revoked_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_api_tokens_token_hash ON api_tokens(token_hash);
