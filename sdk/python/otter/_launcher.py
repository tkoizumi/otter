"""Execute an integration module, then its optional @run entrypoint."""

import runpy
import sys

from .runner import execute_pending


def main() -> None:
    if len(sys.argv) < 2:
        raise SystemExit("usage: python -m otter._launcher <entrypoint> [args...]")
    entrypoint = sys.argv[1]
    sys.argv = sys.argv[1:]
    runpy.run_path(entrypoint, run_name="__main__")
    execute_pending()


if __name__ == "__main__":
    main()
