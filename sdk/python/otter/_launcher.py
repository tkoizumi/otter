"""Execute an integration module, then its optional @run entrypoint."""

import runpy
import sys

from . import _capture
from .runner import execute_pending


def main() -> None:
    if len(sys.argv) < 2:
        raise SystemExit("usage: python -m otter._launcher <entrypoint> [args...]")
    entrypoint = sys.argv[1]
    sys.argv = sys.argv[1:]

    # Instrument before the integration is imported, so a request made at module
    # level is covered as well as one made inside the entrypoint.
    _capture.install_from_environment()
    try:
        runpy.run_path(entrypoint, run_name="__main__")
    except BaseException:
        # A failure in module-level code, or in an integration that uses
        # Context directly, never reaches the runner's handler. Capture is
        # flushed here before the traceback propagates and the interpreter
        # exits, so the recording is not silently left pending.
        _capture.flush_before_exit("the run failed while loading or running module-level code")
        raise
    try:
        execute_pending()
    finally:
        # The success path leaves through normal interpreter shutdown, which runs
        # atexit handlers but not on a failure exit, so it is flushed here too.
        _capture.flush_before_exit()


if __name__ == "__main__":
    main()
