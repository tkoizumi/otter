"""Open resumable product sync windows and finalize their progress."""

from dataclasses import dataclass
from datetime import datetime

from otter_connectors.checkpoint import Watermark

from reporting import report_window_progress


@dataclass(frozen=True)
class SyncWindow:
    """The checkpoint and starting position for this run's window."""

    watermark: Watermark
    start: datetime
    cursor: str | None
    resumed: bool
    had_watermark: bool


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
    return SyncWindow(watermark, start, cursor, resumed, had_watermark)


def finalize_window(log, window, result, started, *, dry_run):
    """Commit a completed window or retain its position for the next run."""
    if result.complete:
        if not dry_run:
            window.watermark.commit(started)
    else:
        window.watermark.save_cursor(result.cursor)

    report_window_progress(log, window.start, result, dry_run=dry_run)
