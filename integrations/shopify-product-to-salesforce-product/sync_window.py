"""Open resumable product sync windows and finalize their progress."""

from dataclasses import dataclass
from datetime import datetime

from otter_connectors.checkpoint import Watermark

from run_state import report_window_progress


@dataclass(frozen=True)
class SyncWindow:
    """The checkpoint, starting position and mode for this run's window.

    Every durable write a window makes goes through this object, so "a dry run
    persists nothing" is stated once rather than at each call site. It used to
    be three call sites with one of them guarded, and the unguarded pair let a
    multi-page rehearsal leave a resume cursor behind.
    """

    watermark: Watermark
    start: datetime
    cursor: str | None
    resumed: bool
    had_watermark: bool
    dry_run: bool

    def checkpoint(self, cursor):
        """Persist the in-window position after a page.

        A dry run keeps nothing: the cursor would make the next real run resume
        past records this run never wrote, and then commit beyond them.
        """
        if not self.dry_run and cursor:
            self.watermark.save_cursor(cursor)

    def close(self, started, *, complete, cursor=None):
        """Advance the committed watermark, or keep the position for next run."""
        if self.dry_run:
            return
        if complete:
            self.watermark.commit(started)
        else:
            self.watermark.save_cursor(cursor)


def begin_window(state, cfg, started) -> SyncWindow:
    """Configure the watermark and open or resume its window."""
    watermark = Watermark(
        state,
        overlap_seconds=cfg.overlap_seconds,
        backfill_from=cfg.backfill_from,
        lookback_days=cfg.backfill_days,
    )
    had_watermark = watermark.committed() is not None
    start, cursor, resumed = watermark.begin(started)
    return SyncWindow(watermark, start, cursor, resumed, had_watermark, cfg.dry_run)


def finalize_window(log, window, result, started):
    """Close the window and report where it ended up."""
    window.close(started, complete=result.complete, cursor=result.cursor)
    report_window_progress(log, window.start, result, dry_run=window.dry_run)
