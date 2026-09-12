"""Resumable sync watermarks, persisted in Otter state.

An incremental sync has to answer one question correctly: *what has already been
processed?* Getting it wrong either loses records or reprocesses them, and the
awkward case is a run that dies half way through a window.

The contract here:

* the committed watermark only advances when a window has been fully drained;
* while a window is in flight its page cursor is persisted, so a crash, a
  timeout or a daemon restart resumes where it stopped rather than starting the
  window over;
* each new window re-scans a small overlap, which costs nothing when writes are
  idempotent and closes the race with records changed while a run was executing.

Usage::

    watermark = Watermark(ctx.state, overlap_seconds=600,
                          backfill_from=parse_iso(os.environ.get("BACKFILL_FROM")))
    window_start, cursor, resumed = watermark.begin(utcnow())
    ...
        watermark.save_cursor(cursor)      # after each page
    ...
    watermark.commit(started)              # only when the window drained
"""

from datetime import timedelta

from .timeutil import parse_iso, to_iso

__all__ = ["DEFAULT_KEYS", "Watermark"]

#: State keys, matching the names the shopify-to-salesforce example has always
#: used so existing deployments keep their position.
DEFAULT_KEYS = {
    "committed": "sync_cursor",
    "window": "in_progress_window_start",
    "cursor": "in_progress_cursor",
}


class Watermark:
    """A resumable position in a source system, backed by an Otter state store."""

    def __init__(self, state, overlap_seconds=600, backfill_from=None,
                 lookback_days=30, keys=None):
        self.state = state
        self.overlap = timedelta(seconds=max(0, overlap_seconds))
        self.backfill_from = backfill_from
        self.lookback_days = max(0, lookback_days)
        self.keys = dict(DEFAULT_KEYS)
        if keys:
            self.keys.update(keys)

    def committed(self):
        """The last fully-processed position, or ``None`` on a first run."""
        return parse_iso(self.state.get(self.keys["committed"]))

    def begin(self, now):
        """Open or reopen a window.

        Returns ``(window_start, cursor, resumed)``: the lower bound to query
        from, the page cursor to continue from (or ``None``), and whether an
        interrupted window is being resumed.
        """
        window = parse_iso(self.state.get(self.keys["window"]))
        cursor = self.state.get(self.keys["cursor"])
        if window is not None and cursor:
            return window, cursor, True

        committed = self.committed()
        if committed is None:
            start = self.backfill_from or (now - timedelta(days=self.lookback_days))
        else:
            start = committed - self.overlap

        self.state.set(self.keys["window"], to_iso(start))
        self.state.delete(self.keys["cursor"])
        return start, None, False

    def save_cursor(self, cursor):
        """Persist progress inside the open window."""
        if cursor:
            self.state.set(self.keys["cursor"], cursor)

    def commit(self, started):
        """Close the window and advance the committed watermark.

        ``started`` is when the run began, not when it finished: anything
        changed while it was executing is then picked up by the next window.
        """
        self.state.set(self.keys["committed"], to_iso(started - self.overlap))
        self.state.delete(self.keys["cursor"])
        self.state.delete(self.keys["window"])

    def reset(self):
        """Forget both the committed watermark and any window in flight."""
        for key in self.keys.values():
            self.state.delete(key)
