#!/bin/sh
# P0-11 — disk pressure is a dead-man's switch, so the check reports by silence.
#
# Claim (CA-33, R-17): four stores grow on a runtime host -- otter.db, the
# .releases snapshots, the prepared environments (environments/ plus the
# managed interpreter in python/) and the uv cache -- and each has a cap, with a
# free-space floor over all four. While every measure is inside its bound the
# check publishes DiskCheck and DiskFreeMB; when one crosses, or when a measure
# cannot be taken at all, the check goes quiet and the alarm over the missing
# datapoint raises the alert. The failure mode is not "slow", it is "the daemon
# cannot write", so silence on the wire is the only report that is still
# available when the host is out of space.
#
# The daemon has no storage surface to poll -- /health answers with status,
# version and uptime, and counts only with a token -- so the subject here is
# the host-level check itself (scripts/disk-check.sh), run against a throwaway
# data directory whose four stores have sizes this script controls. The
# CloudWatch endpoint and IMDSv2 metadata service are stood in for by one local
# HTTP server that records every request it receives, so "did we publish?" is
# read off a log rather than asserted from the script's own opinion. Nothing
# here goes to AWS or to 169.254.169.254.
#
# Falsifiability (DRILL_SABOTAGE=ping-always): replace the threshold logic with
# the naive check that always publishes. The drill must go red on the FIRST
# threshold assertion -- a store over its cap where the check still reported
# health -- because a check that publishes no matter what keeps the alarm quiet
# while the disk fills, which is the failure this task exists to prevent. If
# that run passes, this drill is only testing that a shell script can exit
# non-zero.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
# OTTER_DISK_CHECK_SUBJECT points the drill at a mutated copy of the check, the
# same way scripts/test-disk-check.sh and scripts/test-provision.sh do, so its
# assertions can be falsified without touching the checkout. A mutated copy must
# have otter-metric.sh installed beside it: the check calls its sibling.
subject=${OTTER_DISK_CHECK_SUBJECT:-$root/scripts/disk-check.sh}

[ -f "$subject" ] || {
	echo "disk-pressure: missing $subject" >&2
	exit 2
}
# The check reads only these; a stray value in the caller's environment must not
# change what the drill measures.
unset OTTER_DATA_DIR OTTER_DISK_MAX_DB_MB OTTER_DISK_MAX_RELEASES_MB \
	OTTER_DISK_MAX_ENVIRONMENTS_MB OTTER_DISK_MAX_CACHE_MB OTTER_DISK_MIN_FREE_MB \
	OTTER_METRIC_ENDPOINT OTTER_METRIC_REGION OTTER_METRIC_HOST \
	OTTER_METRIC_TIMEOUT OTTER_IMDS_ENDPOINT 2>/dev/null || true

SABOTAGE=${DRILL_SABOTAGE:-}
case "$SABOTAGE" in
"" | ping-always) ;;
*) echo "disk-pressure: unknown DRILL_SABOTAGE=$SABOTAGE (known: ping-always)" >&2; exit 2 ;;
esac

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-disk-pressure.XXXXXX")
cleaning=0
receiver_pid=""

fail() {
	echo "disk-pressure: FAILED: $1" >&2
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
	[ -n "$receiver_pid" ] && kill "$receiver_pid" 2>/dev/null || true
	if [ "$status" -ne 0 ]; then
		echo "disk-pressure: workspace kept at $work" >&2
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
echo "disk-pressure: receiver $METRIC_URL (a publish is a POST / in its log)"

# --- the four stores, in a throwaway data directory --------------------------
# The sizes are this script's to choose; the caps are derived from what du
# actually reports rather than from the byte counts, because du rounds up to
# the filesystem block and a directory costs a block of its own. The drill is
# testing the comparison, not the block size, so a fixture that measures 3MB
# where 2 MiB was written is still a fixture -- it just moves the caps.
data=$work/data
mkdir -p "$data/.releases" "$data/environments" "$data/python" "$data/cache"
dd if=/dev/zero of="$data/otter.db" bs=1024 count=2048 2>/dev/null
dd if=/dev/zero of="$data/.releases/snapshot.bin" bs=1024 count=2048 2>/dev/null
dd if=/dev/zero of="$data/environments/site-packages.bin" bs=1024 count=1024 2>/dev/null
dd if=/dev/zero of="$data/python/cpython.bin" bs=1024 count=1024 2>/dev/null
dd if=/dev/zero of="$data/cache/wheels.bin" bs=1024 count=2048 2>/dev/null

kib_of() { # what the check's du will see, in KiB
	du -sk "$1" | awk 'NR == 1 { print $1 }'
}
mb_up() { # KiB -> MB, rounded up, the convention the check documents
	echo $((($1 + 1023) / 1024))
}

db_mb=$(mb_up "$(kib_of "$data/otter.db")")
releases_mb=$(mb_up "$(kib_of "$data/.releases")")
environments_mb=$(mb_up "$(($(kib_of "$data/environments") + $(kib_of "$data/python")))")
cache_mb=$(mb_up "$(kib_of "$data/cache")")

# Every store must be at least 1MB, or "one MB below its size" would be a
# negative cap and the cross below would be testing nothing.
for measured in "$db_mb" "$releases_mb" "$environments_mb" "$cache_mb"; do
	[ "$measured" -ge 1 ] || fail "fixture: a store measured ${measured}MB; the four stores must be at least 1MB each"
done

# The in-threshold cap is one MB above the largest store, so every store is
# inside it; a cross undercuts one store's own size by one MB.
cap=$db_mb
for measured in "$releases_mb" "$environments_mb" "$cache_mb"; do
	if [ "$measured" -gt "$cap" ]; then cap=$measured; fi
done
cap=$((cap + 1))
floor=1
echo "disk-pressure: fixture db=${db_mb}MB releases=${releases_mb}MB environments=${environments_mb}MB cache=${cache_mb}MB, every cap ${cap}MB, free-space floor ${floor}MB"

cap_db=0
cap_releases=0
cap_environments=0
cap_cache=0
min_free=0

# inside_caps puts every measure back inside its bound: the control state every
# cross is compared against.
inside_caps() {
	cap_db=$cap
	cap_releases=$cap
	cap_environments=$cap
	cap_cache=$cap
	min_free=$floor
}

# check runs the disk check, or the naive always-publish check under sabotage.
# It reads $data, $METRIC_URL and the caps from this script.
check() {
	if [ "$SABOTAGE" = ping-always ]; then
		curl -fsS --max-time 5 -X POST --data "Action=PutMetricData" "$METRIC_URL/" >/dev/null 2>&1
		return $?
	fi
	OTTER_DATA_DIR=$data \
		OTTER_METRIC_ENDPOINT=$METRIC_URL \
		OTTER_IMDS_ENDPOINT=$IMDS_URL \
		OTTER_METRIC_HOST=disk-pressure-drill \
		OTTER_DISK_MAX_DB_MB=$cap_db \
		OTTER_DISK_MAX_RELEASES_MB=$cap_releases \
		OTTER_DISK_MAX_ENVIRONMENTS_MB=$cap_environments \
		OTTER_DISK_MAX_CACHE_MB=$cap_cache \
		OTTER_DISK_MIN_FREE_MB=$min_free \
		OTTER_METRIC_TIMEOUT=5 \
		sh "$subject"
}

# expect_ping asserts the alarm was told the host is alive, exactly once.
# expect_silence asserts it was told nothing at all, which is what makes the
# alarm fire -- and that the check's own words name the measure that crossed, so
# a silence from some unrelated crash cannot stand in for pressure.
expect_ping() {
	before=$(pings)
	status=0
	out=$(check 2>&1) || status=$?
	after=$(pings)
	if [ -n "$out" ]; then printf '%s\n' "$out"; fi
	[ "$status" -eq 0 ] ||
		fail "$1: the check refused to publish while every measure was inside its bound, so the alarm would fire on a healthy host"
	[ "$after" -eq "$((before + 1))" ] ||
		fail "$1: the check exited 0 but the receiver saw $((after - before)) publishes, not one"
	echo "disk-pressure: ok inside    $1"
}

expect_silence() { # what, why it is correct, text the check must print
	before=$(pings)
	status=0
	out=$(check 2>&1) || status=$?
	after=$(pings)
	if [ -n "$out" ]; then printf '%s\n' "$out"; fi
	[ "$status" -ne 0 ] ||
		fail "$1: the check published, so the alarm stays quiet: $2"
	[ "$after" -eq "$before" ] ||
		fail "$1: the check failed *and* published, so the alarm stays quiet: $2"
	if [ -n "$3" ] && ! printf '%s\n' "$out" | grep -Fq "$3"; then
		fail "$1: the check went silent without naming the measure that crossed (wanted '$3'): $2"
	fi
	echo "disk-pressure: ok silent    $1"
}

# --- 1. every store inside its cap: the check publishes ----------------------
# This is also the control for the four crosses below: the same data directory,
# the same stores, the same receiver, with only the one cap moved.
inside_caps
expect_ping "all four stores under their caps"

# --- 2. one store over its cap: silence --------------------------------------
# Each cross is the control pair to assertion 1: if the check published here, a
# host whose disk is filling would look healthy.
inside_caps
cap_db=$((db_mb - 1))
expect_silence "otter.db at ${db_mb}MB over a $((db_mb - 1))MB cap" \
	"a database over its cap must stop the publishes, or the alarm stays quiet while SQLite runs out of disk" \
	"BREACH otter.db"

inside_caps
cap_releases=$((releases_mb - 1))
expect_silence ".releases at ${releases_mb}MB over a $((releases_mb - 1))MB cap" \
	"release snapshots over their cap must stop the publishes, or a deploy can fill the disk unnoticed" \
	"BREACH .releases"

inside_caps
cap_environments=$((environments_mb - 1))
expect_silence "environments plus python at ${environments_mb}MB over a $((environments_mb - 1))MB cap" \
	"the prepared environments over their cap must stop the publishes, or the store with deferred GC grows unnoticed" \
	"BREACH environments + python"

inside_caps
cap_cache=$((cache_mb - 1))
expect_silence "the uv cache at ${cache_mb}MB over a $((cache_mb - 1))MB cap" \
	"a cache over its cap must stop the publishes, or it takes the headroom the other three stores need" \
	"BREACH cache"

# --- 3. the free-space floor: silence ----------------------------------------
# The control is the run before it: same caps, same stores, a floor of 1MB.
inside_caps
min_free=999999999
expect_silence "a free-space floor of ${min_free}MB" \
	"a host with less free space than the floor must stop the publishes, or the daemon runs out of disk with the alarm still quiet" \
	"BREACH free space"

# --- 4. restored: it publishes again -----------------------------------------
# The control that proves the silence above was the crossed bound and not the
# check having died for some other reason.
inside_caps
expect_ping "every measure restored inside its bound"

# --- 5. a measure that cannot be taken: silence ------------------------------
inside_caps
data_kept=$data
data=$work/does-not-exist
expect_silence "a data directory that does not exist" \
	"a check that cannot see the stores must never report health" \
	"is not a directory"
data=$data_kept

echo "disk-pressure: ok: publishes track the four stores and the free-space floor, and stop when one is crossed"
