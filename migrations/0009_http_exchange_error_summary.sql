-- A bounded reason for a failed exchange, extracted at ingestion.
--
-- The full reason is already stored in response_body, but that is a payload: the
-- timeline and the request list deliberately never read payload columns, so a
-- reader had to open `otter request` to learn why a 400 or 500 happened. These
-- two columns carry a short, sanitized summary instead.
--
-- They are derived from the *sanitized* body, after redaction has run, so this
-- discloses nothing the request detail would not. They are bounded independently
-- of the server's message so a 40 KiB error page cannot become a 40 KiB row.
--
-- The default is the empty string, which means "no summary was extracted". That
-- is also what a run captured under `metadata` records, where there is no body
-- to read: the trace shows the status and says no message was captured rather
-- than implying the response carried none.

ALTER TABLE http_exchanges ADD COLUMN response_error_code TEXT NOT NULL DEFAULT '';
ALTER TABLE http_exchanges ADD COLUMN response_error TEXT NOT NULL DEFAULT '';
