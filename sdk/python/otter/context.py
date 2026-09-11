"""The :class:`Context` handed to every Otter integration.

The daemon starts each integration as a child process and exports:

===========================  =================================================
``OTTER_INTEGRATION_ID``     integration name, e.g. ``counter``
``OTTER_RUN_ID``             UUID of the current run
``OTTER_API_URL``            e.g. ``http://127.0.0.1:7337``
``OTTER_STATE_TOKEN``        per-run bearer token
``OTTER_TRIGGER_TYPE``       ``manual`` | ``cron`` | ``webhook``
``OTTER_INTEGRATION_DIR``    absolute path of the integration directory
===========================  =================================================

Typical use::

    from otter import Context

    ctx = Context.from_environment()
    ctx.log.info("hello", run=ctx.run_id)
    ctx.state.set("last_seen", "now")
"""

import os
from typing import Mapping, Optional

from ._client import Client, OtterError
from .log import Logger
from .state import State
from .trigger import Trigger

__all__ = ["Context"]


class Context:
    """Everything an integration needs to talk to the Otter daemon.

    Attributes:
        run_id: UUID of the current run.
        integration_id: Name of the integration.
        api_url: Base URL of the daemon API.
        integration_dir: Absolute path of the integration directory.
        trigger: :class:`~otter.trigger.Trigger` describing how the run started.
        state: :class:`~otter.state.State` key/value store.
        log: :class:`~otter.log.Logger` structured logger.
    """

    def __init__(
        self,
        run_id: str,
        integration_id: str,
        api_url: str,
        token: Optional[str] = None,
        trigger_type: Optional[str] = None,
        integration_dir: str = "",
        client: Optional[Client] = None,
    ) -> None:
        if not api_url:
            raise OtterError("OTTER_API_URL is required to build an Otter Context")
        self.run_id = run_id
        self.integration_id = integration_id
        self.api_url = str(api_url).strip().rstrip("/")
        self.integration_dir = integration_dir or os.getcwd()
        self._token = token
        self._client = client if client is not None else Client(self.api_url, token=token)
        self.trigger = Trigger(self._client, run_id, trigger_type=trigger_type)
        self.state = State(self._client, integration_id)
        self.log = Logger(self._client, run_id)

    @classmethod
    def from_environment(cls, environ: Optional[Mapping[str, str]] = None) -> "Context":
        """Build a context from the ``OTTER_*`` environment variables.

        Raises :class:`OtterError` when a required variable is missing.
        """
        env = os.environ if environ is None else environ
        api_url = (env.get("OTTER_API_URL") or "").strip()
        if not api_url:
            raise OtterError(
                "OTTER_API_URL is not set; the Otter daemon sets it for every "
                "integration run and Context.from_environment() requires it"
            )
        run_id = (env.get("OTTER_RUN_ID") or "").strip()
        if not run_id:
            raise OtterError("OTTER_RUN_ID is not set; this code only runs inside an Otter run")
        integration_id = (env.get("OTTER_INTEGRATION_ID") or "").strip()
        if not integration_id:
            raise OtterError(
                "OTTER_INTEGRATION_ID is not set; this code only runs inside an Otter run"
            )
        return cls(
            run_id=run_id,
            integration_id=integration_id,
            api_url=api_url,
            token=(env.get("OTTER_STATE_TOKEN") or "").strip() or None,
            trigger_type=(env.get("OTTER_TRIGGER_TYPE") or "").strip() or None,
            integration_dir=(env.get("OTTER_INTEGRATION_DIR") or "").strip(),
        )

    def __repr__(self) -> str:
        return "Context(integration_id=%r, run_id=%r, api_url=%r)" % (
            self.integration_id,
            self.run_id,
            self.api_url,
        )
