"""The ``@run`` decorator.

``@run`` is what turns ``python3 main.py`` into an actual integration run.
Decorating a function executes it immediately, at decoration time, with a
:class:`~otter.context.Context` built from the environment::

    from otter import run

    @run
    def main(ctx):
        ctx.state.set("last_run", "now")

No ``if __name__ == "__main__":`` block is required.

Behaviour:

* When ``OTTER_RUN_ID`` is absent (plain import, unit tests, tooling) the
  decorator is a no-op and returns the function unchanged.
* When ``OTTER_RUN_ID`` is present, the function runs immediately.
* A zero-argument function is supported: the context is only passed when the
  function accepts a positional parameter.
* On success the process exits with status ``0``.
* On failure the full traceback is logged through ``ctx.log.error`` *and*
  written to stderr, then the process exits with status ``1`` so the daemon can
  apply its retry policy.
* Repeated imports in one process cannot execute the same decorated function
  twice.
"""

import inspect
import os
import sys
import threading
import traceback
from typing import Any, Callable, TypeVar

from .context import Context

__all__ = ["run"]

F = TypeVar("F", bound=Callable[..., Any])

#: Attribute set on the ``sys`` module holding the executions claimed so far.
_GUARD_ATTR = "_otter_run_decorator_executed"

_guard_lock = threading.Lock()


def run(fn: F) -> F:
    """Execute ``fn`` now when running under the Otter daemon, else do nothing."""
    run_id = os.environ.get("OTTER_RUN_ID")
    if not run_id:
        # Not running under the daemon: leave the function untouched so the
        # module stays importable and unit-testable.
        return fn
    if not _claim(run_id, fn):
        return fn
    _execute(fn)
    return fn


def _claim(run_id: str, fn: Any) -> bool:
    """Return ``True`` the first time this (run, function) pair is decorated."""
    name = getattr(fn, "__qualname__", None) or getattr(fn, "__name__", "run")
    key = (run_id, str(name))
    with _guard_lock:
        executed = getattr(sys, _GUARD_ATTR, None)
        if executed is None:
            executed = set()
            try:
                setattr(sys, _GUARD_ATTR, executed)
            except Exception:  # pragma: no cover - exotic interpreters
                return True
        if key in executed:
            return False
        executed.add(key)
        return True


def _execute(fn: Any) -> None:
    """Run ``fn`` with a fresh context and translate the outcome into an exit."""
    ctx = None
    try:
        ctx = Context.from_environment()
        _invoke(fn, ctx)
    except SystemExit:
        raise
    except BaseException:
        _report_failure(ctx, traceback.format_exc())
        raise SystemExit(1)
    raise SystemExit(0)


def _invoke(fn: Any, ctx: Context) -> Any:
    """Call ``fn`` with ``ctx`` when it accepts a positional argument, else alone."""
    try:
        signature = inspect.signature(fn)
    except (TypeError, ValueError):
        return fn(ctx)
    accepts_positional = False
    for parameter in signature.parameters.values():
        if parameter.kind in (parameter.POSITIONAL_ONLY, parameter.POSITIONAL_OR_KEYWORD):
            accepts_positional = True
            break
        if parameter.kind == parameter.VAR_POSITIONAL:
            accepts_positional = True
            break
    if accepts_positional:
        return fn(ctx)
    return fn()


def _report_failure(ctx: Any, tb: str) -> None:
    """Log and print a traceback without ever raising."""
    stripped = tb.strip()
    summary = stripped.splitlines()[-1] if stripped else "integration failed"
    if ctx is not None:
        try:
            ctx.log.error(
                "integration failed: %s" % summary,
                error=summary,
                traceback=tb,
            )
        except BaseException:
            pass
    try:
        sys.stderr.write(tb if tb.endswith("\n") else tb + "\n")
        sys.stderr.flush()
    except Exception:
        pass
