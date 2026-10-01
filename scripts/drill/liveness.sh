#!/bin/sh
# P0-10 — liveness detection is a dead-man's switch, not a positive check.
#
# Claim (CA-31, R-05): when the runtime stops answering, the heartbeat stops
# reporting, so an alarm raised by the *absence* of its metric fires without
# anyone watching a terminal.
#
# The runtime under test is a real `otterd` from this checkout on a loopback
# port. The CloudWatch endpoint and IMDSv2 metadata service are stood in for by
# one local HTTP server that records every request it receives, so "did we
# publish?" is read off a log rather than asserted from the script's own
# opinion. It answers the IMDSv2 routes as the instance would -- the token PUT
# needs its TTL header, the credential reads need the token -- so the publisher
# is exercised through the same sequence it uses on a host, with nothing going
# to AWS or to 169.254.169.254.
#
# Falsifiability (DRILL_SABOTAGE=ping-always): replace the check with the naive
# positive version that publishes whether or not the runtime is healthy. The
# drill must go red on the FIRST assertion, because a positive check publishes
# into the void and keeps the alarm quiet while the runtime is dead. If that run
# passes, this drill is only testing that a shell script can exit non-zero.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
OTTERD_BIN=${OTTERD_BIN:-$root/bin/otterd}

[ -x "$OTTERD_BIN" ] || { echo "liveness: not executable: $OTTERD_BIN (run make build)" >&2; exit 2; }
unset OTTER_API_URL OTTER_API_TOKEN OTTER_METRIC_ENDPOINT OTTER_METRIC_REGION \
	OTTER_METRIC_HOST OTTER_METRIC_TIMEOUT OTTER_IMDS_ENDPOINT 2>/dev/null || true

SABOTAGE=${DRILL_SABOTAGE:-}
case "$SABOTAGE" in
"" | ping-always) ;;
*) echo "liveness: unknown DRILL_SABOTAGE=$SABOTAGE (known: ping-always)" >&2; exit 2 ;;
esac

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-liveness.XXXXXX")
cleaning=0
daemon_pid=""
receiver_pid=""

fail() {
	echo "liveness: FAILED: $1" >&2
	exit 1
}

free_port() {
	python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
}

# pings reads the receiver's log. The count is what the alarm would see: only a
# POST / carries a metric, so the IMDS reads around it do not count.
pings() {
	count=$(grep -c '^POST ' "$work/receiver.log" 2>/dev/null || true)
	echo "${count:-0}"
}

cleanup() {
	status=$?
	[ "$cleaning" -eq 1 ] && return
	cleaning=1
	[ -n "$daemon_pid" ] && kill "$daemon_pid" 2>/dev/null || true
	[ -n "$receiver_pid" ] && kill "$receiver_pid" 2>/dev/null || true
	if [ "$status" -ne 0 ]; then
		echo "liveness: workspace kept at $work" >&2
		[ -f "$work/daemon.log" ] && tail -n 20 "$work/daemon.log" >&2 || true
	else
		rm -rf "$work"
	fi
}
trap cleanup EXIT INT TERM

# --- the stand-in CloudWatch endpoint and IMDS -------------------------------
# One server answers both: the IMDSv2 routes under /latest/ and the
# PutMetricData POST at /. It records each request in its log, and refuses a
# credential read that does not carry the token, the way the instance does.
cat >"$work/receiver.py" <<'PY'
import http.server, json, os, sys

port = int(sys.argv[1])
log = os.environ["OTTER_STANDIN_LOG"]
role = "otter-metric-role"


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def body(self):
        try:
            n = int(self.headers.get("Content-Length", "0") or 0)
        except ValueError:
            n = 0
        return self.rfile.read(n) if n > 0 else b""

    def record(self, payload=b""):
        with open(log, "a") as f:
            f.write("%s %s\n" % (self.command, self.path))
            if payload:
                f.write(payload.decode("utf-8", "replace") + "\n")

    def reply(self, status, payload=b"", ctype="text/plain"):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        if payload:
            self.wfile.write(payload)

    def do_PUT(self):
        self.record()
        if self.path == "/latest/api/token" and \
                self.headers.get("X-aws-ec2-metadata-token-ttl-seconds"):
            self.reply(200, b"standin-imds-token")
            return
        self.reply(400)

    def do_GET(self):
        self.record()
        path = self.path.split("?")[0]
        if path == "/__ready":
            self.reply(200, b"ok")
            return
        if not self.headers.get("X-aws-ec2-metadata-token"):
            self.reply(401)
            return
        if path == "/latest/meta-data/placement/region":
            self.reply(200, b"us-east-1")
            return
        if path == "/latest/meta-data/iam/security-credentials/":
            self.reply(200, role.encode())
            return
        if path.startswith("/latest/meta-data/iam/security-credentials/"):
            self.reply(200, json.dumps({
                "Code": "Success",
                "AccessKeyId": "AKIA-STANDIN-ACCESS-KEY",
                "SecretAccessKey": "standin-secret-access-key",
                "Token": "standin-session-token",
            }).encode(), "application/json")
            return
        self.reply(404)

    def do_POST(self):
        self.record(self.body())
        self.reply(200, b"<PutMetricDataResponse/>", "text/xml")


http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY

receiver_port=$(free_port)
OTTER_STANDIN_LOG=$work/receiver.log python3 "$work/receiver.py" "$receiver_port" >"$work/receiver.out" 2>&1 &
receiver_pid=$!
METRIC_URL="http://127.0.0.1:$receiver_port"
IMDS_URL="http://127.0.0.1:$receiver_port"
ready=0
for _ in $(seq 1 50); do
	if curl -s -o /dev/null --max-time 1 "$METRIC_URL/__ready"; then ready=1; break; fi
	sleep 0.2
done
[ "$ready" -eq 1 ] || fail "the stand-in metric endpoint did not start"
echo "liveness: receiver $METRIC_URL (a publish is a POST / in its log)"

# check runs the heartbeat, or the naive positive check under sabotage. It reads
# $API, $METRIC_URL and $IMDS_URL from the environment of this script.
check() {
	if [ "$SABOTAGE" = ping-always ]; then
		curl -fsS --max-time 5 -X POST --data "Action=PutMetricData" "$METRIC_URL/" >/dev/null 2>&1
		return $?
	fi
	OTTER_API_URL=$API \
		OTTER_METRIC_ENDPOINT=$METRIC_URL \
		OTTER_IMDS_ENDPOINT=$IMDS_URL \
		OTTER_METRIC_HOST=liveness-drill \
		OTTER_METRIC_TIMEOUT=5 \
		sh "$root/scripts/heartbeat.sh"
}

# --- 1. no runtime: silence, and a non-zero exit -----------------------------
dead_port=$(free_port)
API="http://127.0.0.1:$dead_port"
before=$(pings)
if check; then
	fail "the check succeeded although nothing was listening on $API"
fi
after=$(pings)
[ "$after" -eq "$before" ] || fail "the check published although the runtime was down: the alarm would never fire"
echo "liveness: down      no runtime, no publish, non-zero exit"

# --- 2. a real runtime: it publishes -----------------------------------------
api_port=$(free_port)
API="http://127.0.0.1:$api_port"
# The daemon needs a jobs root it can scan; an empty one answers /health, and
# keeping it under $work is part of why this drill writes nothing into the
# checkout.
mkdir -p "$work/jobs"
"$OTTERD_BIN" --data "$work/data" --jobs "$work/jobs" --listen "127.0.0.1:$api_port" >"$work/daemon.log" 2>&1 &
daemon_pid=$!
up=0
for _ in $(seq 1 60); do
	if curl -fsS --max-time 1 "$API/health" >/dev/null 2>&1; then up=1; break; fi
	kill -0 "$daemon_pid" 2>/dev/null || break
	sleep 0.5
done
[ "$up" -eq 1 ] || fail "otterd did not answer $API/health"
before=$(pings)
check || fail "the check failed against a healthy runtime answering $API/health"
after=$(pings)
[ "$after" -gt "$before" ] || fail "the check did not publish for a healthy runtime"
echo "liveness: up        real otterd on $api_port, metric delivered"

# --- 3. the runtime dies: the publishes stop ---------------------------------
kill "$daemon_pid" 2>/dev/null || true
wait "$daemon_pid" 2>/dev/null || true
daemon_pid=""
before=$(pings)
if check; then
	fail "the check succeeded after the runtime was killed"
fi
after=$(pings)
[ "$after" -eq "$before" ] || fail "the check published after the runtime was killed"
echo "liveness: killed    publishes stopped, so the alarm fires"

echo "liveness: ok: publishes track the runtime and stop when it dies"
