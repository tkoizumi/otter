"""Unit tests for the Otter Python SDK.

Run with::

    python3 -m unittest discover -s sdk/python/tests

No daemon and no third-party packages are required. Every test talks to a fake
in-process HTTP server (``http.server`` on ``127.0.0.1:0``) that emulates the
part of the Otter API the SDK uses, or to a subprocess running a tiny
integration script.
"""

import io
import json
import os
import tempfile
import socket
import subprocess
import sys
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
SDK_ROOT = os.path.dirname(HERE)
if SDK_ROOT not in sys.path:
    sys.path.insert(0, SDK_ROOT)

from otter import Context, OtterError, run  # noqa: E402
from otter._client import Client  # noqa: E402
from otter.log import Logger  # noqa: E402
from otter.trigger import Trigger  # noqa: E402

RUN_ID = "11111111-2222-3333-4444-555555555555"
# The identity and the label are deliberately different values, so a test that
# confuses them fails.
INTEGRATION_ID = "0195a7c2-8e31-7b64-9f02-6dcb482ea510"
INTEGRATION_NAME = "counter"
TOKEN = "secret-token"


def free_port():
    """Return a TCP port that nothing is listening on."""
    sock = socket.socket()
    try:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]
    finally:
        sock.close()


class FakeDaemon:
    """A tiny stand-in for ``otterd`` implementing the endpoints the SDK uses."""

    def __init__(self):
        self.state = {}
        self.run = {
            "id": RUN_ID,
            "integration_id": INTEGRATION_ID,
            "trigger_type": "webhook",
            "status": "running",
            "attempt": 1,
            "created_at": "2024-01-01T00:00:00Z",
            "started_at": "2024-01-01T00:00:00Z",
            "finished_at": None,
            "exit_code": None,
            "error": None,
            "metadata": None,
        }
        self.logs = []
        self.counts = {}
        self.fail_state = False
        self.fail_logs = False
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), self._make_handler())
        self.port = self.server.server_address[1]
        self.url = "http://127.0.0.1:%d" % self.port
        self.thread = threading.Thread(
            target=self.server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True
        )
        self.thread.start()

    # -- lifecycle ------------------------------------------------------

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def count(self, path):
        return self.counts.get(path, 0)

    def paths(self):
        """Every request path seen so far, for asserting how state is addressed."""
        return list(self.counts)

    # -- request handling -----------------------------------------------

    def _make_handler(self):
        daemon = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, fmt, *args):  # silence the test output
                pass

            def do_GET(self):
                daemon._handle(self, "GET")

            def do_PUT(self):
                daemon._handle(self, "PUT")

            def do_POST(self):
                daemon._handle(self, "POST")

            def do_DELETE(self):
                daemon._handle(self, "DELETE")

        return Handler

    def _handle(self, handler, method):
        parsed = urllib.parse.urlsplit(handler.path)
        segments = [urllib.parse.unquote(part) for part in parsed.path.strip("/").split("/")]
        body = None
        length = int(handler.headers.get("Content-Length") or 0)
        if length:
            raw = handler.rfile.read(length)
            try:
                body = json.loads(raw.decode("utf-8"))
            except ValueError:
                body = raw.decode("utf-8", "replace")
        self.counts[parsed.path] = self.counts.get(parsed.path, 0) + 1
        self.last_auth = handler.headers.get("Authorization")
        self.last_content_type = handler.headers.get("Content-Type")
        status, payload = self._route(method, segments, body)
        self._send(handler, status, payload)

    def _route(self, method, segments, body):
        not_found = {"error": {"code": "not_found", "message": "not found"}}
        if segments[:2] == ["v1", "runs"] and len(segments) == 3 and method == "GET":
            if self.run and segments[2] == self.run.get("id"):
                return 200, self.run
            return 404, not_found
        if (
            segments[:2] == ["v1", "runs"]
            and len(segments) == 4
            and segments[3] == "logs"
            and method == "POST"
        ):
            if self.fail_logs:
                return 500, {"error": {"code": "internal", "message": "log sink down"}}
            self.logs.append(body)
            return 201, {"status": "logged"}
        if segments[:2] == ["v1", "integrations"] and len(segments) >= 4 and segments[3] == "state":
            if self.fail_state:
                return 500, {"error": {"code": "internal", "message": "state store down"}}
            integration_id = segments[2]
            if len(segments) == 4 and method == "GET":
                return 200, {"integration_id": integration_id, "state": dict(self.state)}
            if len(segments) == 5:
                key = segments[4]
                if method == "GET":
                    if key in self.state:
                        return 200, self.state[key]
                    return 404, {
                        "error": {"code": "not_found", "message": "state key %r is unset" % key}
                    }
                if method == "PUT":
                    self.state[key] = body
                    return 200, {
                        "integration_id": integration_id,
                        "key": key,
                        "value": body,
                        "updated_at": "2024-01-01T00:00:00Z",
                    }
                if method == "DELETE":
                    if key in self.state:
                        del self.state[key]
                        return 200, {
                            "integration_id": integration_id,
                            "key": key,
                            "deleted": True,
                        }
                    return 404, {
                        "error": {"code": "not_found", "message": "state key %r is unset" % key}
                    }
        return 404, not_found

    @staticmethod
    def _send(handler, status, payload):
        data = b"" if payload is None else json.dumps(payload).encode("utf-8")
        handler.send_response(status)
        if payload is not None:
            handler.send_header("Content-Type", "application/json")
        handler.send_header("Content-Length", str(len(data)))
        handler.end_headers()
        if data:
            handler.wfile.write(data)


class SDKTestCase(unittest.TestCase):
    """Base class with a fake daemon, a client and a context."""

    def setUp(self):
        self.daemon = FakeDaemon()
        self.addCleanup(self.daemon.close)
        self.client = Client(self.daemon.url, token=TOKEN, timeout=5.0)
        # Keep retries instant in tests.
        self.client.retry_delays = (0, 0)
        self.ctx = Context.from_environment(self.env())

    def env(self, **overrides):
        values = {
            "OTTER_INTEGRATION_ID": INTEGRATION_ID,
            "OTTER_INTEGRATION_NAME": INTEGRATION_NAME,
            "OTTER_RUN_ID": RUN_ID,
            "OTTER_API_URL": self.daemon.url,
            "OTTER_STATE_TOKEN": TOKEN,
            "OTTER_TRIGGER_TYPE": "webhook",
            "OTTER_INTEGRATION_DIR": "/tmp/counter",
        }
        values.update(overrides)
        return values


class StateTests(SDKTestCase):
    def test_json_round_trip(self):
        self.ctx.state.set("count", 3)
        self.assertEqual(self.ctx.state.get("count"), 3)
        self.assertEqual(self.ctx.state.set("count", 4), 4)
        self.assertEqual(self.ctx.state.get("count"), 4)

        payload = {"nested": {"list": [1, 2, 3], "ok": True, "nothing": None}}
        self.ctx.state.set("payload", payload)
        self.assertEqual(self.ctx.state.get("payload"), payload)

        self.ctx.state.set("text", "abc")
        self.assertEqual(self.ctx.state.get("text"), "abc")

        self.ctx.state.set("ns:key.with-chars_1", [1, 2])
        self.assertEqual(self.ctx.state.get("ns:key.with-chars_1"), [1, 2])

        self.assertEqual(
            self.ctx.state.all(),
            {
                "count": 4,
                "payload": payload,
                "text": "abc",
                "ns:key.with-chars_1": [1, 2],
            },
        )

    def test_missing_key_returns_default(self):
        sentinel = object()
        self.assertIs(self.ctx.state.get("nope", sentinel), sentinel)
        self.assertIsNone(self.ctx.state.get("nope"))

    def test_delete_reports_existence(self):
        self.ctx.state.set("count", 1)
        self.assertIs(self.ctx.state.delete("count"), True)
        self.assertIs(self.ctx.state.delete("count"), False)
        self.assertIsNone(self.ctx.state.get("count"))

    def test_invalid_key_raises(self):
        for bad in ("", "has space", "a" * 129, "slash/inside"):
            with self.assertRaises(OtterError):
                self.ctx.state.get(bad)

    def test_non_serializable_value_raises(self):
        with self.assertRaises(OtterError):
            self.ctx.state.set("bad", object())
        with self.assertRaises(OtterError):
            self.ctx.state.set("bad", float("nan"))

    def test_api_error_raises_ottererror(self):
        self.daemon.fail_state = True
        with self.assertRaises(OtterError):
            self.ctx.state.get("count")
        with self.assertRaises(OtterError):
            self.ctx.state.set("count", 1)
        with self.assertRaises(OtterError):
            self.ctx.state.delete("count")
        with self.assertRaises(OtterError):
            self.ctx.state.all()

    def test_bearer_token_and_content_type_are_sent(self):
        self.ctx.state.set("count", 1)
        self.assertEqual(self.daemon.last_auth, "Bearer %s" % TOKEN)
        self.assertEqual(self.daemon.last_content_type, "application/json")


class ClientTests(SDKTestCase):
    def test_retries_transient_500(self):
        self.daemon.fail_state = True
        status, body = self.client.get_json("/v1/integrations/%s/state/x" % INTEGRATION_ID)
        self.assertEqual(status, 500)
        self.assertEqual(body["error"]["code"], "internal")
        self.assertEqual(self.daemon.count("/v1/integrations/%s/state/x" % INTEGRATION_ID), 3)

    def test_does_not_retry_404(self):
        status, body = self.client.get_json("/v1/integrations/%s/state/missing" % INTEGRATION_ID)
        self.assertEqual(status, 404)
        self.assertEqual(body["error"]["code"], "not_found")
        self.assertEqual(
            self.daemon.count("/v1/integrations/%s/state/missing" % INTEGRATION_ID), 1
        )

    def test_transport_failure_raises_ottererror(self):
        client = Client("http://127.0.0.1:%d" % free_port(), token=TOKEN, timeout=1.0)
        client.retry_delays = (0, 0)
        with self.assertRaises(OtterError):
            client.get_json("/v1/runs/%s" % RUN_ID)


class TriggerTests(SDKTestCase):
    def test_webhook_trigger_from_metadata(self):
        self.daemon.run["metadata"] = {
            "type": "webhook",
            "body": {"a": 1},
            "headers": {"Content-Type": ["application/json"], "X-Token": ["abc"]},
        }
        self.assertEqual(self.ctx.trigger.type, "webhook")
        self.assertEqual(self.ctx.trigger.body, {"a": 1})
        self.assertEqual(
            self.ctx.trigger.headers,
            {"Content-Type": ["application/json"], "X-Token": ["abc"]},
        )
        self.assertEqual(self.ctx.trigger.header("content-type"), "application/json")
        self.assertEqual(self.ctx.trigger.header("x-token"), "abc")
        self.assertIsNone(self.ctx.trigger.header("missing"))
        self.assertEqual(self.ctx.trigger.metadata["type"], "webhook")

    def test_manual_trigger_has_no_body(self):
        ctx = Context.from_environment(self.env(OTTER_TRIGGER_TYPE="manual"))
        self.daemon.run["metadata"] = {"type": "manual"}
        self.assertEqual(ctx.trigger.type, "manual")
        self.assertIsNone(ctx.trigger.body)
        self.assertEqual(ctx.trigger.headers, {})

    def test_type_falls_back_to_metadata(self):
        trigger = Trigger(self.client, RUN_ID, trigger_type=None)
        self.daemon.run["metadata"] = {"type": "cron"}
        self.assertEqual(trigger.type, "cron")

    def test_type_unknown_without_any_information(self):
        trigger = Trigger(self.client, RUN_ID, trigger_type="nonsense")
        self.daemon.run["metadata"] = None
        self.assertEqual(trigger.type, "unknown")

    def test_metadata_is_fetched_lazily_and_cached(self):
        self.daemon.run["metadata"] = {"type": "webhook", "body": {"a": 1}}
        trigger = Trigger(self.client, RUN_ID, trigger_type="webhook")
        self.assertEqual(self.daemon.count("/v1/runs/%s" % RUN_ID), 0)
        self.assertEqual(trigger.body, {"a": 1})
        self.assertEqual(self.daemon.count("/v1/runs/%s" % RUN_ID), 1)
        self.assertEqual(trigger.headers, {})
        self.assertEqual(self.daemon.count("/v1/runs/%s" % RUN_ID), 1)

    def test_metadata_fetch_failure_degrades_gracefully(self):
        trigger = Trigger(Client("http://127.0.0.1:%d" % free_port(), timeout=1.0), RUN_ID)
        self.assertEqual(trigger.metadata, {})
        self.assertIsNone(trigger.body)
        self.assertEqual(trigger.headers, {})
        self.assertEqual(trigger.type, "unknown")


class LoggerTests(SDKTestCase):
    def test_posts_structured_log_record(self):
        self.ctx.log.info("Counter executed", count=1)
        self.assertEqual(
            self.daemon.logs[-1],
            {
                "stream": "otter",
                "message": "Counter executed",
                "fields": {"count": 1, "level": "info"},
            },
        )

    def test_every_level_and_warn_alias(self):
        self.ctx.log.debug("d")
        self.ctx.log.info("i")
        self.ctx.log.warning("w")
        self.ctx.log.warn("w2")
        self.ctx.log.error("e")
        levels = [entry["fields"]["level"] for entry in self.daemon.logs]
        self.assertEqual(levels, ["debug", "info", "warning", "warning", "error"])

    def test_falls_back_to_stderr_and_never_raises(self):
        self.daemon.fail_logs = True
        stream = io.StringIO()
        logger = Logger(self.client, RUN_ID, fallback_stream=stream)
        logger.error("boom", code=500)
        line = stream.getvalue().strip()
        self.assertEqual(
            json.loads(line),
            {"level": "error", "message": "boom", "fields": {"code": 500}},
        )

    def test_fallback_survives_unserializable_fields(self):
        self.daemon.fail_logs = True
        stream = io.StringIO()
        logger = Logger(self.client, RUN_ID, fallback_stream=stream)
        logger.info("weird", thing=object())
        self.assertIn("weird", stream.getvalue())

    def test_logging_never_raises_without_daemon(self):
        client = Client("http://127.0.0.1:%d" % free_port(), timeout=1.0)
        client.retry_delays = (0, 0)
        stream = io.StringIO()
        Logger(client, RUN_ID, fallback_stream=stream).info("offline")
        self.assertIn("offline", stream.getvalue())


class ContextTests(SDKTestCase):
    def test_from_environment(self):
        ctx = Context.from_environment(self.env())
        self.assertEqual(ctx.run_id, RUN_ID)
        self.assertEqual(ctx.integration_id, INTEGRATION_ID)
        self.assertEqual(ctx.name, INTEGRATION_NAME)
        self.assertEqual(ctx.api_url, self.daemon.url)
        self.assertEqual(ctx.integration_dir, "/tmp/counter")
        self.assertEqual(ctx.trigger.type, "webhook")

    def test_name_falls_back_to_the_identity(self):
        """A daemon that predates OTTER_INTEGRATION_NAME still yields a label."""
        env = self.env()
        del env["OTTER_INTEGRATION_NAME"]
        ctx = Context.from_environment(env)
        self.assertEqual(ctx.name, INTEGRATION_ID)

    def test_state_is_namespaced_by_the_identity_not_the_label(self):
        ctx = Context.from_environment(self.env())
        ctx.state.set("count", 1)
        paths = self.daemon.paths()
        self.assertTrue(
            any(INTEGRATION_ID in path for path in paths),
            "state was not addressed by the identity: %r" % (paths,),
        )
        self.assertFalse(
            any(path.endswith("/integrations/%s/state/count" % INTEGRATION_NAME) for path in paths),
            "state was addressed by the label: %r" % (paths,),
        )

    def test_missing_api_url_raises_ottererror(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            with self.assertRaises(OtterError) as caught:
                Context.from_environment()
        self.assertIn("OTTER_API_URL", str(caught.exception))

    def test_missing_run_id_raises_ottererror(self):
        env = self.env()
        del env["OTTER_RUN_ID"]
        with self.assertRaises(OtterError) as caught:
            Context.from_environment(env)
        self.assertIn("OTTER_RUN_ID", str(caught.exception))

    def test_invalid_api_url_raises_ottererror(self):
        with self.assertRaises(OtterError):
            Context(run_id=RUN_ID, integration_id=INTEGRATION_ID, api_url="")


class RunDecoratorTests(SDKTestCase):
    def test_noop_without_run_id(self):
        called = []

        with mock.patch.dict(os.environ, {}, clear=True):

            @run
            def main(ctx):
                called.append(ctx)

        self.assertEqual(called, [])
        self.assertEqual(main.__name__, "main")
        self.assertTrue(callable(main))

    def _subprocess(self, source, **env_overrides):
        env = {
            "OTTER_INTEGRATION_ID": INTEGRATION_ID,
            "OTTER_INTEGRATION_NAME": INTEGRATION_NAME,
            "OTTER_RUN_ID": RUN_ID,
            "OTTER_API_URL": self.daemon.url,
            "OTTER_STATE_TOKEN": TOKEN,
            "OTTER_TRIGGER_TYPE": "manual",
            "OTTER_INTEGRATION_DIR": HERE,
            "PYTHONPATH": SDK_ROOT + os.pathsep + os.environ.get("PYTHONPATH", ""),
            "PATH": os.environ.get("PATH", ""),
        }
        env.update(env_overrides)
        with tempfile.TemporaryDirectory() as directory:
            script = os.path.join(directory, "main.py")
            with open(script, "w", encoding="utf-8") as output:
                output.write(source)
            return subprocess.run(
                [sys.executable, "-m", "otter._launcher", script],
                env=env,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                universal_newlines=True,
                timeout=60,
            )

    def test_executes_one_argument_function_and_exits_zero(self):
        result = self._subprocess(
            "from otter import run\n"
            "@run\n"
            "def main(ctx):\n"
            "    print('RAN-ONE', ctx.name)\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        # The label, not the opaque identity, is what a decorator's own output
        # should show.
        self.assertIn("RAN-ONE counter", result.stdout)

    def test_executes_zero_argument_function_and_exits_zero(self):
        result = self._subprocess(
            "from otter import run\n"
            "@run\n"
            "def main():\n"
            "    print('RAN-ZERO')\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("RAN-ZERO", result.stdout)

    def test_failure_logs_and_exits_nonzero(self):
        result = self._subprocess(
            "from otter import run\n"
            "@run\n"
            "def main(ctx):\n"
            "    raise RuntimeError('boom')\n"
        )
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("RuntimeError: boom", result.stderr)
        self.assertTrue(self.daemon.logs, "expected a log record for the failure")
        record = self.daemon.logs[-1]
        self.assertEqual(record["stream"], "otter")
        self.assertEqual(record["fields"]["level"], "error")
        self.assertIn("RuntimeError: boom", record["message"])

    def test_guard_prevents_double_execution(self):
        result = self._subprocess(
            "from otter import run\n"
            "SRC = '''\n"
            "from otter import run\n"
            "@run\n"
            "def main(ctx):\n"
            "    print('EXEC')\n"
            "'''\n"
            "for name in ('first', 'second'):\n"
            "    try:\n"
            "        exec(compile(SRC, name, 'exec'), {'__name__': name})\n"
            "    except SystemExit:\n"
            "        pass\n"
            "print('DONE')\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("DONE", result.stdout)
        self.assertEqual(result.stdout.count("EXEC"), 1)

    def test_helpers_defined_below_main_are_available(self):
        # Regression: executing at decoration time made any name defined
        # further down the file unavailable, which is a very natural way to
        # write an integration.
        result = self._subprocess(
            "from otter import run\n"
            "@run\n"
            "def main(ctx):\n"
            "    print('HELPER-SAYS', helper())\n"
            "def helper():\n"
            "    return 'ok'\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("HELPER-SAYS ok", result.stdout)

    def test_runs_only_after_the_module_body_finishes(self):
        result = self._subprocess(
            "from otter import run\n"
            "@run\n"
            "def main(ctx):\n"
            "    print('INTEGRATION')\n"
            "print('MODULE-BODY')\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("MODULE-BODY", result.stdout)
        self.assertIn("INTEGRATION", result.stdout)
        self.assertLess(
            result.stdout.index("MODULE-BODY"),
            result.stdout.index("INTEGRATION"),
            "the decorated function must run after the module body completes",
        )

    def test_second_decorated_function_is_ignored_with_a_warning(self):
        result = self._subprocess(
            "from otter import run\n"
            "@run\n"
            "def first(ctx):\n"
            "    print('FIRST')\n"
            "@run\n"
            "def second(ctx):\n"
            "    print('SECOND')\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("FIRST", result.stdout)
        self.assertNotIn("SECOND", result.stdout)
        self.assertIn("ignoring @run on second", result.stderr)

    def test_systemexit_from_main_controls_the_status(self):
        result = self._subprocess(
            "from otter import run\n"
            "@run\n"
            "def main(ctx):\n"
            "    print('BEFORE-EXIT')\n"
            "    raise SystemExit(3)\n"
        )
        self.assertEqual(result.returncode, 3, result.stderr)
        self.assertIn("BEFORE-EXIT", result.stdout)

    def test_module_failure_does_not_invoke_entrypoint(self):
        result = self._subprocess(
            "from otter import run\n"
            "@run\n"
            "def main(ctx):\n"
            "    print('SHOULD-NOT-RUN')\n"
            "raise RuntimeError('module failed')\n"
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("SHOULD-NOT-RUN", result.stdout)
        self.assertIn("module failed", result.stderr)

    def test_thread_pool_can_initialize_in_entrypoint(self):
        result = self._subprocess(
            "from otter import run\n"
            "@run\n"
            "def main(ctx):\n"
            "    from concurrent.futures import ThreadPoolExecutor\n"
            "    with ThreadPoolExecutor(1) as pool:\n"
            "        print(pool.submit(lambda: 42).result())\n"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("42", result.stdout)


if __name__ == "__main__":
    unittest.main()
