"""Otter Python SDK.

A thin client for integrations that run as child processes of the ``otterd``
daemon. Standard library only; the daemon owns all state.

Public API::

    from otter import Context, run, OtterError

    @run
    def main(ctx):
        count = ctx.state.get("count", 0) + 1
        ctx.log.info("Counter executed", count=count)
        ctx.state.set("count", count)
"""

from ._client import Client, OtterError
from .context import Context
from .log import Logger
from .runner import run
from .state import State
from .trigger import Trigger

__version__ = "0.1.0"

__all__ = [
    "Context",
    "run",
    "OtterError",
    "Client",
    "Logger",
    "State",
    "Trigger",
]
