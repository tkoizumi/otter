#!/bin/sh
# Drill: rotating a job credential takes a restart, and what each kind of work
# sees is exactly what the ops guide says (CA-13).
#
# Why this exists: docs/operations.md tells the operator to edit `otter.env` and
# restart the runtime, and `otter reload` deliberately does not re-read
# credentials. Both statements are promises about behavior that nothing executed.
# This drill is their executable form:
#
#   1. a run sees the credential that was configured when the daemon started;
#   2. `otter reload` does NOT pick up a rotated value -- reload replaces what
#      the daemon knows about jobs, not its process environment;
#   3. a restart does, and the next run sees the new value;
#   4. removing the credential makes the run fail *before Python starts*, with
#      the secret named, which is the configuration failure the guide documents.
#
# What it proves:
#   * the value a run observes is the daemon's environment at start, not the
#     file on disk at run time;
#   * rotation is applied by restart, and by nothing else;
#   * a missing credential is a fast, named failure rather than a silent empty
#     string handed to the job.
#
# Second half (the broker). The environment provider above is the DEFAULT and
# has no live credentials, so a rotation reaches a run only through a restart.
# With a broker configured the runtime resolves per RUN instead, and the same
# rotation reaches the very next run with NO restart and NO reload -- D2's
# "rotation without redeploy", which is the property CA-13 asks for. Both halves
# are asserted here, because "a rotation needs a restart" is only true of one of
# them and the docs must not say it unconditionally.
#
# What it explicitly does NOT prove:
#   * that a child already executing observes the old value across a rotation.
#     Under restart rotation no child survives: an in-flight run is interrupted
#     by the restart, which `operations.md` documents and the crash-recovery
#     tests cover. The broker half rotates between runs and makes no claim about
#     a run mid-flight (the unit test `TestARunKeepsTheValueItResolved` does).
#   * the real AWS path. The broker's Secrets Manager endpoint and its instance
#     role are both stood in for on loopback, so what is exercised is the
#     runtime's per-run fetch, not IAM or the network.
#
# Falsifiability (DRILL_SABOTAGE). The clean run must be green. Two modes:
#
#   reload-applies  writes the rotated value into the *daemon's* environment via
#                   `otter reload` before asserting step 2, so the "reload did
#                   not apply the rotation" assertion goes red.
#   stale-broker    makes the stand-in backend serve the document it first read,
#                   so the broker half's "the next run sees the rotated value"
#                   assertion goes red. Without this the broker half could pass
#                   while testing nothing.
set -eu

OTTER_BIN=${OTTER_BIN:?OTTER_BIN must be an absolute path to the otter binary}
case "$OTTER_BIN" in
/*) ;;
*)
	echo "drill: OTTER_BIN must be an absolute path, got $OTTER_BIN" >&2
	exit 2
	;;
esac
[ -x "$OTTER_BIN" ] || {
	echo "drill: not executable: $OTTER_BIN" >&2
	exit 2
}

SABOTAGE=${DRILL_SABOTAGE:-}
case "$SABOTAGE" in
"" | reload-applies | stale-broker) ;;
*)
	echo "drill: unknown DRILL_SABOTAGE=$SABOTAGE" >&2
	echo "drill: valid modes: reload-applies, stale-broker" >&2
	exit 2
	;;
esac

# A developer's shell must not redirect this run at another daemon, and must not
# provide the credential the workspace file is supposed to provide.
unset OTTER_API_URL OTTER_API_TOKEN 2>/dev/null || true
unset OTTER_DATA_DIR OTTER_JOBS_DIR OTTER_LISTEN 2>/dev/null || true
unset OTTER_PROJECT_ROOT OTTER_SERVE_DIR 2>/dev/null || true
unset ROTATE_TOKEN 2>/dev/null || true

JOB=rotate-job
SECRET=ROTATE_TOKEN
FIRST=first-token-value
SECOND=second-token-value

tmpbase=${TMPDIR:-/tmp}
while [ "$tmpbase" != "${tmpbase%/}" ]; do tmpbase=${tmpbase%/}; done
[ -n "$tmpbase" ] || tmpbase=/tmp
WORK=$(mktemp -d "$tmpbase/otter-drill-secret-rotation.XXXXXX")
WS="$WORK/ws"
START_EPOCH=$(date +%s)
cleaning=0
BROKER_PID=""
FAKE_PID=""

say() { echo "drill: $*"; }
fail() {
	echo "drill: FAILED: $*" >&2
	exit 1
}

cleanup() {
	status=$?
	[ "$cleaning" -eq 1 ] && return
	cleaning=1

	( cd "$WS" 2>/dev/null && "$OTTER_BIN" stop >/dev/null 2>&1 ) || true
	# The broker and its stand-in backend are background processes this drill
	# started; leaving either alive would collide with the next run's ports.
	[ -n "${BROKER_PID:-}" ] && kill "$BROKER_PID" 2>/dev/null || true
	[ -n "${FAKE_PID:-}" ] && kill "$FAKE_PID" 2>/dev/null || true

	if [ "$status" -ne 0 ]; then
		echo "drill: failed; last daemon log lines:" >&2
		tail -n 40 "$WS/.otter/serve/serve.log" >&2 2>/dev/null || true
		echo "drill: workspace was $WORK" >&2
	fi
	echo "drill: elapsed $(( $(date +%s) - START_EPOCH ))s" >&2

	if [ -n "${OTTER_DRILL_KEEP:-}" ] || { [ "$status" -ne 0 ] && [ -z "${CI:-}" ]; }; then
		echo "drill: keeping $WORK" >&2
	else
		rm -rf "$WORK"
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM

otter() {
	( cd "$WS" && "$OTTER_BIN" "$@" )
}

wait_ready() {
	label=$1
	i=0
	until otter status >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt 600 ] || fail "$label runtime did not become ready"
		sleep 0.1
	done
}

wait_stopped() {
	i=0
	while otter status >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt 600 ] || fail "the runtime did not stop"
		sleep 0.1
	done
}

wait_run() {
	id=$1
	i=0
	status=""
	while [ "$i" -lt 600 ]; do
		status=$(otter run-status "$id" 2>/dev/null | sed -n 's/^status: *//p' || true)
		case "$status" in
		succeeded | failed | timed_out | cancelled)
			printf '%s' "$status"
			return 0
			;;
		esac
		i=$((i + 1))
		sleep 0.1
	done
	fail "run $id did not settle (last status: ${status:-unknown})"
}

# run_and_token runs the job, waits for it and echoes the credential the child
# recorded through ctx.state.
run_and_token() {
	id=$(otter run --no-wait "$JOB")
	status=$(wait_run "$id")
	[ "$status" = "succeeded" ] || fail "run $id settled as $status"
	# State stores JSON, so a string value comes back quoted; strip the quotes.
	otter state get "$JOB" token 2>/dev/null | sed 's/^"//; s/"$//' || true
}

write_env() {
	printf '%s=%s\n' "$SECRET" "$1" >"$WS/otter.env"
}

mkdir -p "$WS"

# 1. a workspace whose job declares a secret and records what it saw.
say "init        scaffolding $JOB with a declared secret"
otter init "$JOB" >/dev/null
cat >>"$WS/$JOB/otter.yaml" <<'YAML'

secrets:
  - ROTATE_TOKEN
YAML
cat >"$WS/$JOB/main.py" <<'PY'
"""Record the credential this run was handed, so the drill can read it back."""

import os

from otter import Context

ctx = Context.from_environment()
token = os.environ.get("ROTATE_TOKEN", "")

ctx.log.info("token seen", token=token)
ctx.state.set("token", token)
PY

say "config      writing $SECRET to otter.env"
write_env "$FIRST"
otter validate "$JOB" >/dev/null
otter release "$JOB" >/dev/null

# 2. the value a run sees is the daemon's environment at start.
say "start       bringing the runtime up"
otter start --detach >/dev/null
wait_ready "initial"
GOT=$(run_and_token)
[ "$GOT" = "$FIRST" ] || fail "the first run saw '${GOT:-nothing}', want the configured value"
say "run         the child saw the configured credential"

# 3. rotating the file and reloading does not change what a run sees.
say "rotate      writing the new value and reloading"
write_env "$SECOND"
if [ "$SABOTAGE" = "reload-applies" ]; then
	say "sabotage: restarting so reload appears to apply the rotation"
	otter stop >/dev/null 2>&1 || true
	wait_stopped
	otter start --detach >/dev/null
	wait_ready "sabotage"
fi
otter reload >/dev/null || fail "otter reload failed"
GOT=$(run_and_token)
[ "$GOT" = "$FIRST" ] ||
	fail "otter reload applied the rotated credential (saw '$GOT'); operations.md says reload does not re-read credentials -- update both"
say "reload      a run still sees the value the daemon started with"

# 4. a restart applies the rotation.
say "restart     stopping and starting the daemon"
otter stop >/dev/null 2>&1 || true
wait_stopped
otter start --detach >/dev/null
wait_ready "after the rotation"
GOT=$(run_and_token)
[ "$GOT" = "$SECOND" ] || fail "after the restart the run saw '${GOT:-nothing}', want the rotated value"
say "rotated     a run after the restart sees the new credential"

# 5. removing the credential is a named configuration failure, before Python.
say "remove      taking the credential out of otter.env"
: >"$WS/otter.env"
otter stop >/dev/null 2>&1 || true
wait_stopped
otter start --detach >/dev/null
wait_ready "without the credential"
RUN=$(otter run --no-wait "$JOB")
STATUS=$(wait_run "$RUN")
[ "$STATUS" = "failed" ] || fail "a run with no configured credential settled as $STATUS, want failed"
OUT=$(otter run-status "$RUN")
printf '%s\n' "$OUT" | grep -q "$SECRET" ||
	fail "the failure does not name the missing secret: $OUT"
printf '%s\n' "$OUT" | grep -qi "requires secrets that are not available" ||
	fail "the failure is not the documented configuration error: $OUT"
say "missing     the run failed naming $SECRET, before Python started"

# ------------------------------------------------- rotation WITH a broker
#
# Everything above is the environment provider: no live credentials, so a
# rotation reaches a run only through a restart. This half configures a broker,
# so the runtime resolves per RUN and a rotation needs neither a restart nor a
# reload.

command -v python3 >/dev/null 2>&1 || {
	echo "drill: python3 is required to stand in for IMDS and Secrets Manager" >&2
	exit 2
}

FAKE_DIR="$WORK/fake"
mkdir -p "$FAKE_DIR"
free_port() {
	python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}
FAKE_PORT=$(free_port)
BROKER_PORT=$(free_port)
BROKER_RUNTIME=rt_drill
BROKER_CREDENTIAL=drill-broker-credential
BROKER_TOKEN=broker-first-token
BROKER_ROTATED=broker-second-token

# The namespace is the RUNTIME and the JOB, so the durable job id is part of the
# path -- a label would not do, and the broker validates it as a path segment.
JOB_ID=$(otter --json jobs | python3 -c '
import json, sys
jobs = json.load(sys.stdin)["jobs"]
print(next(job["id"] for job in jobs if job["name"] == "rotate-job"))
')
SECRET_NAMESPACE="otter/$BROKER_RUNTIME/$JOB_ID/$SECRET"
say "broker      secret namespace is $SECRET_NAMESPACE"

cat >"$FAKE_DIR/fake.py" <<'PY'
"""Stand in for IMDSv2 and AWS Secrets Manager on ONE loopback port.

The broker reads its AWS credentials from IMDS and a job's secrets from Secrets
Manager, and a drill may reach neither. This answers both.

It re-reads its secrets file on every request unless FAKE_CACHE=1, which serves
the first document it ever read. That is DRILL_SABOTAGE=stale-broker, and it is
what makes the rotation assertion below falsifiable: without it the assertion
could pass while testing nothing.
"""

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

directory = sys.argv[1]
port = int(sys.argv[2])
secrets_path = os.path.join(directory, "secrets.json")
cache = os.environ.get("FAKE_CACHE") == "1"
_cached = None


def secrets():
    global _cached
    if cache:
        if _cached is None:
            with open(secrets_path) as handle:
                _cached = json.load(handle)
        return _cached
    with open(secrets_path) as handle:
        return json.load(handle)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _send(self, status, payload, content_type="application/json"):
        body = payload.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_PUT(self):
        if self.path == "/latest/api/token":
            if not self.headers.get("X-aws-ec2-metadata-token-ttl-seconds"):
                self._send(400, "the token request needs its TTL header", "text/plain")
                return
            self._send(200, "fake-imds-token", "text/plain")
            return
        self._send(404, "no such route", "text/plain")

    def do_GET(self):
        # IMDSv2: a read without the token is refused, as on a real instance.
        if self.headers.get("X-aws-ec2-metadata-token") != "fake-imds-token":
            self._send(401, "token required", "text/plain")
            return
        if self.path == "/latest/meta-data/iam/security-credentials/":
            self._send(200, "otter-agent", "text/plain")
            return
        if self.path == "/latest/meta-data/iam/security-credentials/otter-agent":
            self._send(200, json.dumps({
                "AccessKeyId": "AKIDDRILL",
                "SecretAccessKey": "drill-secret-access-key",
                "Token": "drill-session-token",
                "Expiration": "2030-01-01T00:00:00Z",
            }), "text/plain")
            return
        self._send(404, "no such route", "text/plain")

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(length) or b"{}")
        target = self.headers.get("X-Amz-Target", "")
        values = secrets()

        if target == "secretsmanager.ListSecrets":
            prefix = ""
            for entry in body.get("Filters", []):
                if entry.get("Key") == "name" and entry.get("Values"):
                    prefix = entry["Values"][0]
            names = sorted(name for name in values if name.startswith(prefix))
            self._send(200, json.dumps({"SecretList": [{"Name": name} for name in names]}))
            return

        if target == "secretsmanager.BatchGetSecretValue":
            found = [
                {"Name": name, "SecretString": values[name]}
                for name in body.get("SecretIdList", [])
                if name in values
            ]
            self._send(200, json.dumps({"SecretValues": found}))
            return

        self._send(400, json.dumps({"__type": "UnknownOperationException"}))


HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY

printf '{"%s": "%s"}\n' "$SECRET_NAMESPACE" "$BROKER_TOKEN" >"$FAKE_DIR/secrets.json"

# wait_port polls a loopback port, so a background process is not assumed ready.
wait_port() {
	label=$1
	port=$2
	pid=$3
	i=0
	while [ "$i" -lt 100 ]; do
		if python3 -c '
import socket, sys
s = socket.socket()
s.settimeout(0.2)
s.connect(("127.0.0.1", int(sys.argv[1])))
s.close()
' "$port" 2>/dev/null; then
			return 0
		fi
		kill -0 "$pid" 2>/dev/null || fail "$label exited before it was ready"
		i=$((i + 1))
		sleep 0.1
	done
	fail "$label did not become ready on port $port"
}

if [ "$SABOTAGE" = "stale-broker" ]; then
	say "sabotage: the backend will serve the document it first read"
	FAKE_CACHE=1 python3 "$FAKE_DIR/fake.py" "$FAKE_DIR" "$FAKE_PORT" >"$FAKE_DIR/fake.log" 2>&1 &
else
	python3 "$FAKE_DIR/fake.py" "$FAKE_DIR" "$FAKE_PORT" >"$FAKE_DIR/fake.log" 2>&1 &
fi
FAKE_PID=$!
wait_port "the stand-in backend" "$FAKE_PORT" "$FAKE_PID"

printf '{"%s": "%s"}\n' "$BROKER_RUNTIME" "$BROKER_CREDENTIAL" >"$FAKE_DIR/broker-credentials.json"
chmod 600 "$FAKE_DIR/broker-credentials.json"
: >"$FAKE_DIR/tenants.conf"

say "broker      starting the host broker"
OTTER_IMDS_BASE="http://127.0.0.1:$FAKE_PORT" "$OTTER_BIN" pool broker \
	--tenants "$FAKE_DIR/tenants.conf" \
	--listen "127.0.0.1:$BROKER_PORT" \
	--region eu-west-1 \
	--secrets-endpoint "http://127.0.0.1:$FAKE_PORT" \
	--credentials "$FAKE_DIR/broker-credentials.json" \
	>"$FAKE_DIR/broker.log" 2>&1 &
BROKER_PID=$!
wait_port "the broker" "$BROKER_PORT" "$BROKER_PID"

say "broker      pointing the runtime at the broker"
cat >"$WS/otter.env" <<ENV
OTTER_RUNTIME_ID=$BROKER_RUNTIME
OTTER_SECRET_BROKER_URL=http://127.0.0.1:$BROKER_PORT
OTTER_BROKER_CREDENTIAL=$BROKER_CREDENTIAL
ENV
otter stop >/dev/null 2>&1 || true
wait_stopped
otter start --detach >/dev/null
wait_ready "with a broker"

GOT=$(run_and_token)
[ "$GOT" = "$BROKER_TOKEN" ] ||
	fail "the brokered run saw '${GOT:-nothing}', want the value the secret store holds"
say "run         the child saw the value the broker returned"

# The rotation. Deliberately NO `otter stop`, NO `otter start`, NO `otter
# reload`: the runtime resolves per run, so the next run must see the new value.
say "rotate      rewriting the value in the secret store (no restart, no reload)"
printf '{"%s": "%s"}\n' "$SECRET_NAMESPACE" "$BROKER_ROTATED" >"$FAKE_DIR/secrets.json"

GOT=$(run_and_token)
if [ "$GOT" != "$BROKER_ROTATED" ]; then
	tail -n 20 "$FAKE_DIR/broker.log" >&2 2>/dev/null || true
	tail -n 20 "$FAKE_DIR/fake.log" >&2 2>/dev/null || true
	fail "the next run saw '${GOT:-nothing}' after a rotation with no restart; with a broker configured the runtime resolves per run, so the rotation must reach it"
fi
say "rotated     the next run saw the new value with no restart and no reload"

otter stop >/dev/null 2>&1 || true
wait_stopped
kill "$BROKER_PID" 2>/dev/null || true
kill "$FAKE_PID" 2>/dev/null || true
BROKER_PID=""
FAKE_PID=""

say "ok: with the environment provider a rotation needs a restart; with a broker it reaches the next run with none; and a missing credential still fails loudly"
