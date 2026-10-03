"""The :class:`Context` handed to every Otter job.

The daemon starts each job as a child process and exports:

===========================  =================================================
``OTTER_JOB_ID``     durable identity, e.g. ``0195a7c2-8e31-...``
``OTTER_JOB_NAME``   manifest label, e.g. ``counter``
``OTTER_RUN_ID``             UUID of the current run
``OTTER_API_URL``            e.g. ``http://127.0.0.1:7337``
``OTTER_STATE_TOKEN``        per-run bearer token
``OTTER_TRIGGER_TYPE``       ``manual`` | ``cron`` | ``webhook``
``OTTER_JOB_DIR``    absolute path of the job directory
``OTTER_CONFIG``             the job configuration the run pinned, as JSON
===========================  =================================================

The identity and the label are different values on purpose. State, runs and
credentials are namespaced by the identity, which never changes; the label is
what a human reads and may change or be shared with another job. Log
the name, address state with the identity -- which is what :class:`Context`
does for you.

Typical use::

    from otter import Context

    ctx = Context.from_environment()
    ctx.log.info("hello", job=ctx.name, run=ctx.run_id)
    ctx.state.set("last_seen", "now")
"""

import json
import os
from typing import Mapping, Optional

from ._client import Client, OtterError
from .log import Logger
from .state import State
from .trigger import Trigger

__all__ = ["Context"]


def _parse_config(raw: Optional[str]) -> dict:
    """Parse OTTER_CONFIG into the dict a job reads as ``ctx.config``.

    Missing means an empty configuration. A value that is present but not a JSON
    object is a daemon bug rather than a job's problem, so it raises rather than
    silently running with no configuration: a job that would have used a value
    should fail loudly.
    """
    if not raw or not raw.strip():
        return {}
    try:
        parsed = json.loads(raw)
    except ValueError as exc:
        raise OtterError("OTTER_CONFIG is not valid JSON: %s" % exc) from exc
    if not isinstance(parsed, dict):
        raise OtterError("OTTER_CONFIG must be a JSON object, got %s" % type(parsed).__name__)
    return parsed


class Context:
    """Everything a job needs to talk to the Otter daemon.

    Attributes:
        run_id: UUID of the current run.
        job_id: Durable identity of the job. This is the
            namespace its state and credentials belong to; it is not a
            human-readable name.
        name: Manifest label, for logs and messages. May change, and may be
            shared with another job.
        api_url: Base URL of the daemon API.
        job_dir: Absolute path of the job directory.
        trigger: :class:`~otter.trigger.Trigger` describing how the run started.
        state: :class:`~otter.state.State` key/value store.
        log: :class:`~otter.log.Logger` structured logger.
        config: The job configuration the run pinned at submission, as a dict.
            It is ``{}`` when the job has none. Configuration values are
            deployment inputs, not secrets: a secret belongs in ``otter.env``.
    """

    def __init__(
        self,
        run_id: str,
        job_id: str,
        api_url: str,
        token: Optional[str] = None,
        trigger_type: Optional[str] = None,
        job_dir: str = "",
        name: str = "",
        client: Optional[Client] = None,
        config: Optional[Mapping[str, object]] = None,
    ) -> None:
        if not api_url:
            raise OtterError("OTTER_API_URL is required to build an Otter Context")
        self.run_id = run_id
        self.job_id = job_id
        # A caller that predates OTTER_JOB_NAME still gets something
        # printable rather than an empty string.
        self.name = name or job_id
        self.api_url = str(api_url).strip().rstrip("/")
        self.job_dir = job_dir or os.getcwd()
        self.config = dict(config or {})
        self._token = token
        self._client = client if client is not None else Client(self.api_url, token=token)
        self.trigger = Trigger(self._client, run_id, trigger_type=trigger_type)
        self.state = State(self._client, job_id)
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
                "job run and Context.from_environment() requires it"
            )
        run_id = (env.get("OTTER_RUN_ID") or "").strip()
        if not run_id:
            raise OtterError("OTTER_RUN_ID is not set; this code only runs inside an Otter run")
        job_id = (env.get("OTTER_JOB_ID") or "").strip()
        if not job_id:
            raise OtterError(
                "OTTER_JOB_ID is not set; this code only runs inside an Otter run"
            )
        return cls(
            run_id=run_id,
            job_id=job_id,
            name=(env.get("OTTER_JOB_NAME") or "").strip(),
            api_url=api_url,
            token=(env.get("OTTER_STATE_TOKEN") or "").strip() or None,
            trigger_type=(env.get("OTTER_TRIGGER_TYPE") or "").strip() or None,
            job_dir=(env.get("OTTER_JOB_DIR") or "").strip(),
            config=_parse_config(env.get("OTTER_CONFIG")),
        )

    # Compatibility for code in saved releases. New code uses job_id/job_dir.
    @property
    def integration_id(self) -> str:
        return self.job_id

    @property
    def integration_dir(self) -> str:
        return self.job_dir

    def __repr__(self) -> str:
        return "Context(job_id=%r, name=%r, run_id=%r, api_url=%r)" % (
            self.job_id,
            self.name,
            self.run_id,
            self.api_url,
        )
