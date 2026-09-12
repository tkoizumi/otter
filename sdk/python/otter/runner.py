"""The ``@run`` decorator.

``@run`` is what turns ``python3 main.py`` into an actual integration run::

    from otter import run

    @run
    def main(ctx):
        sync(ctx, fetch_cursor(ctx))   # helpers may be defined below


    def sync(ctx, cursor):             # perfectly fine
        ...

No ``if __name__ == "__main__":`` block is required.

Execution is deferred until the module has **finished** loading, not performed
at decoration time. That matters: decoration happens part-way through the file,
so running there would make any name defined further down -- helper functions,
constants, classes -- unavailable. Deferring means a decorated ``main`` behaves
like a normal program entry point.

Behaviour:

* When ``OTTER_RUN_ID`` is absent (plain import, unit tests, tooling) the
  decorator is a no-op and returns the function unchanged.
* When ``OTTER_RUN_ID`` is present, the first decorated function is executed
  once the module body has finished executing.
* A zero-argument function is supported: the context is only passed when the
  function accepts a positional parameter.
* On success the process exits with status ``0``.
* On failure the full traceback is logged through ``ctx.log.error`` *and*
  written to stderr, then the process exits with status ``1`` so the daemon can
  apply its retry policy.
* Repeated imports in one process cannot execute the same decorated function
  twice, and a second ``@run`` in one module is ignored with a warning rather
  than being run ambiguously.
"""

import atexit
import inspect
import os
import sys
import threading
import traceback
from typing import Any, Callable, Optional, TypeVar

from .context import Context

__all__ = ["run"]

F = TypeVar("F", bound=Callable[..., Any])

#: Attribute set on the ``sys`` module holding the executions claimed so far.
_GUARD_ATTR = "_otter_run_decorator_executed"

#: Attribute set on the ``sys`` module holding the function waiting to run.
_PENDING_ATTR = "_otter_run_pending"

_guard_lock = threading.Lock()


def run(fn: F) -> F:
    """Schedule ``fn`` to run when the module finishes loading, under Otter."""
    run_id = os.environ.get("OTTER_RUN_ID")
    if not run_id:
        # Not running under the daemon: leave the function untouched so the
        # module stays importable and unit-testable.
        return fn

    if not _claim(run_id, fn):
        return fn

    if not _enqueue(fn):
        _warn_duplicate(fn)
        return fn

    atexit.register(_run_pending)
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


def _enqueue(fn: Any) -> bool:
    """Remember ``fn`` as the function to run. False if one is already queued."""
    with _guard_lock:
        if getattr(sys, _PENDING_ATTR, None) is not None:
            return False
        try:
            setattr(sys, _PENDING_ATTR, fn)
        except Exception:  # pragma: no cover - exotic interpreters
            return True
        return True


def _take_pending() -> Optional[Any]:
    with _guard_lock:
        fn = getattr(sys, _PENDING_ATTR, None)
        try:
            setattr(sys, _PENDING_ATTR, None)
        except Exception:  # pragma: no cover
            pass
        return fn


def _warn_duplicate(fn: Any) -> None:
    name = getattr(fn, "__qualname__", None) or getattr(fn, "__name__", "run")
    try:
        sys.stderr.write(
            "otter: ignoring @run on %s: this module already has a decorated "
            "entry point, and only one can run per execution\n" % name
        )
        sys.stderr.flush()
    except Exception:
        pass


def _run_pending() -> None:
    """atexit handler: run the queued function and set the process status."""
    fn = _take_pending()
    if fn is None:
        return

    ctx = None
    try:
        ctx = Context.from_environment()
        _invoke(fn, ctx)
    except SystemExit as exc:
        code = exc.code
        if code is None or code == 0:
            return
        _exit_now(code if isinstance(code, int) else 1)
    except BaseException:
        _report_failure(ctx, traceback.format_exc())
        _exit_now(1)

    # Success: fall through so the interpreter exits normally with status 0 and
    # any handlers the integration registered still run.


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


def _exit_now(code: int) -> None:
    """Flush and exit with an exact status, bypassing the rest of shutdown."""
    try:
        sys.stdout.flush()
    except Exception:
        pass
    try:
        sys.stderr.flush()
    except Exception:
        pass
    os._exit(code)
