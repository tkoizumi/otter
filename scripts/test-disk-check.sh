#!/bin/sh
# Edge cases for scripts/disk-check.sh.
#
# The drill (scripts/drill/disk-pressure.sh) proves the dead-man property: over
# a cap, under the free-space floor, or unmeasurable, the check goes silent. It
# deliberately derives its caps from whatever du reports, so it cannot pin the
# boundary arithmetic -- a drill that moved with the subject would certify any
# rounding at all. This test fixes the numbers instead:
#
#   * a store exactly AT its cap passes, and a store one byte over it fails;
#   * a threshold that is not a whole number of MB fails loudly, rather than
#     silently passing or silently becoming "no cap";
#   * free space is compared with >= the floor, not >: free space equal to the
#     floor passes, and one MB less fails;
#   * free space rounds down, so a shortfall smaller than a whole MB still
#     counts against the floor (an implementation that rounded up would claim
#     headroom the host does not have);
#   * a metric endpoint that cannot be reached is a loud failure, not a silent
#     skip;
#   * a data directory that does not exist is a measurement failure, while a
#     *store* that does not exist yet is 0MB and still publishes -- the runtime
#     creates .releases, environments/, python/ and cache/ lazily, and a fresh
#     host is not a host under pressure.
#
# Where the fixture is: `du` is real (real files, real blocks). `df` is a tap in
# the test's own bin directory that reports a fixed Available when the test asks
# for one and passes through to the real df otherwise. Free space cannot be set
# to an exact value on a live filesystem, and "exactly at the floor" is the one
# case that tells >= from >, so a stand-in is the only honest way to pin it. The
# sizes need no stand-in: a file of exactly 1 MiB and a file of 1 MiB + 1 byte
# are exact on both ext4 and APFS (du rounds up to the block, so the second
# measures 1028 KiB), and the fixture asserts the first of those before it is
# used, so a filesystem where it is untrue fails the test loudly instead of
# quietly testing a different number.
#
# The CloudWatch endpoint and IMDSv2 metadata service are stood in for by one
# local HTTP server that records every request, so a "publish" is a POST in its
# log and nothing here reaches AWS or 169.254.169.254.
#
#   sh scripts/test-disk-check.sh
#
# OTTER_DISK_CHECK_SUBJECT points the matrix at a mutated copy of the script,
# the same way scripts/test-provision.sh does. A mutated copy must have
# otter-metric.sh installed beside it: the check calls its sibling.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
subject=${OTTER_DISK_CHECK_SUBJECT:-$here/disk-check.sh}

[ -f "$subject" ] || {
	echo "test-disk-check: missing $subject" >&2
	exit 2
}

real_df=$(command -v df) || real_df=
[ -n "$real_df" ] || {
	echo "test-disk-check: no df on PATH" >&2
	exit 2
}

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-disk-check-test.XXXXXX")
receiver_pid=""
cleaning=0
passed=0
failed=0

# fatal is for a fixture or a setup problem: the matrix cannot run, so it must
# not report a verdict at all. fail is for a case that ran and was wrong.
fatal() {
	echo "test-disk-check: $1" >&2
	exit 2
}

fail() {
	failed=$((failed + 1))
	echo "test-disk-check: FAIL: $1" >&2
}

show() {
	printf '%s\n' "$1" | sed 's/^/    /' >&2
}

# --- the stand-in CloudWatch endpoint and IMDS -------------------------------
# The subject reports health by publishing a metric and reports pressure by
# publishing nothing, so every case here has to count publishes, not just read
# an exit status. One server answers both the IMDSv2 routes under /latest/ and
# the PutMetricData POST at /, and records each request in its log.
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

free_port() {
	python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
}

receiver_port=$(free_port)
OTTER_STANDIN_LOG=$work/receiver.log python3 "$work/receiver.py" "$receiver_port" >"$work/receiver.out" 2>&1 &
receiver_pid=$!
METRIC_URL="http://127.0.0.1:$receiver_port"
IMDS_URL="http://127.0.0.1:$receiver_port"
# A port nothing listens on, for the case that proves an unreachable endpoint is
# fatal rather than a silent skip.
dead_port=$(free_port)
DEAD_URL="http://127.0.0.1:$dead_port"
ready=0
for _ in $(seq 1 50); do
	if curl -s -o /dev/null --max-time 1 "$METRIC_URL/__ready"; then ready=1; break; fi
	sleep 0.2
done
[ "$ready" -eq 1 ] || fatal "the stand-in metric endpoint did not start"

pings() {
	count=$(grep -c '^POST ' "$work/receiver.log" 2>/dev/null || true)
	echo "${count:-0}"
}

cleanup() {
	status=$?
	[ "$cleaning" -eq 1 ] && return
	cleaning=1
	[ -n "$receiver_pid" ] && kill "$receiver_pid" 2>/dev/null || true
	if [ "$status" -ne 0 ] || [ "$failed" -ne 0 ]; then
		echo "test-disk-check: workspace kept at $work" >&2
	else
		rm -rf "$work"
	fi
}
trap cleanup EXIT HUP INT TERM

# --- the df tap ---------------------------------------------------------------
# Reports a fixed Available (in KiB) when OTTER_TEST_FREE_KIB is set; otherwise
# it is the real df, which is what the cases without the override exercise.
mkdir -p "$work/bin"
cat >"$work/bin/df" <<SHIM
#!/bin/sh
if [ -n "\${OTTER_TEST_FREE_KIB:-}" ]; then
	printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n'
	printf '/dev/stub 4194304 2097152 %s 50%% /stub\n' "\$OTTER_TEST_FREE_KIB"
	exit 0
fi
exec "$real_df" "\$@"
SHIM
chmod +x "$work/bin/df"

# --- the stores ---------------------------------------------------------------
data=$work/data
mkdir -p "$data/.releases" "$data/environments" "$data/python" "$data/cache"
at_file=$data/otter.db
dd if=/dev/zero of="$at_file" bs=1048576 count=1 2>/dev/null
dd if=/dev/zero of="$data/.releases/snapshot.bin" bs=1024 count=2048 2>/dev/null
dd if=/dev/zero of="$data/environments/site-packages.bin" bs=1024 count=1024 2>/dev/null
dd if=/dev/zero of="$data/python/cpython.bin" bs=1024 count=1024 2>/dev/null
dd if=/dev/zero of="$data/cache/wheels.bin" bs=1024 count=2048 2>/dev/null

kib_of() {
	du -sk "$1" | awk 'NR == 1 { print $1 }'
}

kib=$(kib_of "$at_file")
[ "$kib" -eq 1024 ] ||
	fatal "fixture: a 1 MiB file measures ${kib}KiB here; 'exactly at the cap' needs a block size that divides 1 MiB"

releases_kib=$(kib_of "$data/.releases")
releases_mb=$(((releases_kib + 1023) / 1024))
[ "$releases_mb" -ge 1 ] || fatal "fixture: .releases measures ${releases_mb}MB"

# --- the matrix ---------------------------------------------------------------
reset_env() {
	OTTER_DATA_DIR=$data
	OTTER_METRIC_ENDPOINT=$METRIC_URL
	OTTER_IMDS_ENDPOINT=$IMDS_URL
	OTTER_METRIC_HOST=disk-check-test
	OTTER_METRIC_TIMEOUT=5
	OTTER_DISK_MAX_DB_MB=64
	OTTER_DISK_MAX_RELEASES_MB=64
	OTTER_DISK_MAX_ENVIRONMENTS_MB=64
	OTTER_DISK_MAX_CACHE_MB=64
	OTTER_DISK_MIN_FREE_MB=1
	export OTTER_DATA_DIR OTTER_METRIC_ENDPOINT OTTER_IMDS_ENDPOINT \
		OTTER_METRIC_HOST OTTER_METRIC_TIMEOUT \
		OTTER_DISK_MAX_DB_MB OTTER_DISK_MAX_RELEASES_MB \
		OTTER_DISK_MAX_ENVIRONMENTS_MB OTTER_DISK_MAX_CACHE_MB \
		OTTER_DISK_MIN_FREE_MB
	unset OTTER_TEST_FREE_KIB 2>/dev/null || true
}

run_subject() {
	PATH="$work/bin:$PATH" sh "$subject" 2>&1
}

# expect NAME want one|none SUBSTRING [VAR=value ...]
#
# `one` means the check must exit 0 and deliver exactly one publish; `none`
# means it must exit non-zero and deliver none. Both halves matter: a check that
# failed for the wrong reason, or that published anyway, is not the behaviour
# the alarm depends on.
expect() {
	name=$1
	want=$2
	must=$3
	shift 3
	reset_env
	for assignment in "$@"; do
		export "$assignment"
	done

	before=$(pings)
	status=0
	out=$(run_subject) || status=$?
	after=$(pings)
	delta=$((after - before))

	if [ "$want" = one ]; then
		if [ "$status" -ne 0 ]; then
			fail "$name: want a delivered publish, got exit $status"
			show "$out"
			return
		fi
		if [ "$delta" -ne 1 ]; then
			fail "$name: want one publish, the receiver saw $delta"
			show "$out"
			return
		fi
	else
		if [ "$status" -eq 0 ]; then
			fail "$name: want a non-zero exit and no publish, got exit 0"
			show "$out"
			return
		fi
		if [ "$delta" -ne 0 ]; then
			fail "$name: want no publish, the receiver saw $delta"
			show "$out"
			return
		fi
	fi
	if [ -n "$must" ] && ! printf '%s\n' "$out" | grep -Fq "$must"; then
		fail "$name: output does not contain the expected text"
		echo "    want substring: $must" >&2
		show "$out"
		return
	fi
	passed=$((passed + 1))
	echo "ok: $name"
}

echo "test-disk-check: receiver $METRIC_URL, subject $subject"

# --- the size boundary -------------------------------------------------------
# otter.db is one file, so there is no directory block in the measure: 1 MiB of
# bytes is 1024 KiB. The cap is 1MB, so the measure is exactly at it.
expect "otter.db exactly at its 1MB cap passes" one "" "OTTER_DISK_MAX_DB_MB=1"

# One byte more pushes du to 1028 KiB (block-rounded), which is 2MB once it is
# rounded up, so the same cap now fails.
printf x >>"$at_file"
kib=$(kib_of "$at_file")
[ "$kib" -gt 1024 ] || fatal "fixture: appending one byte did not grow the measure (still ${kib}KiB)"
expect "otter.db one byte over its 1MB cap fails" none "BREACH otter.db" "OTTER_DISK_MAX_DB_MB=1"
# The control: the same file is inside a 2MB cap, so the failure above was the
# cap and not a broken fixture.
expect "otter.db at the next whole MB passes" one "" "OTTER_DISK_MAX_DB_MB=2"

# A directory store carries its own block, so the cap that is exactly at its
# measure is derived from the measure rather than from the byte count.
expect ".releases exactly at its cap passes" one "" "OTTER_DISK_MAX_RELEASES_MB=$releases_mb"
expect ".releases one MB over its cap fails" none "BREACH .releases" \
	"OTTER_DISK_MAX_RELEASES_MB=$((releases_mb - 1))"

# --- free space: >= the floor, rounded down ----------------------------------
# The tap reports 2048 MiB exactly, which is equal to the floor: it must pass. A
# comparison written with > instead of >= fails here, and that is the point.
expect "free space exactly at the floor passes" one "" \
	"OTTER_TEST_FREE_KIB=2097152" "OTTER_DISK_MIN_FREE_MB=2048"
expect "free space one MB below the floor fails" none "BREACH free space" \
	"OTTER_TEST_FREE_KIB=2097152" "OTTER_DISK_MIN_FREE_MB=2049"
# 2048 MiB + 1 KiB is not 2049 MiB of headroom. The control pair is the case
# below: the tap does not move, only the floor does.
expect "free space rounds down, so a sub-MB shortfall still fails" none "BREACH free space" \
	"OTTER_TEST_FREE_KIB=2097153" "OTTER_DISK_MIN_FREE_MB=2049"
expect "the same tap passes a floor it does meet" one "" \
	"OTTER_TEST_FREE_KIB=2097153" "OTTER_DISK_MIN_FREE_MB=2048"
# The real df is exercised by every case without OTTER_TEST_FREE_KIB: this is
# the one that says so out loud.
expect "real free space above a 1MB floor passes" one "" "OTTER_DISK_MIN_FREE_MB=1"

# --- a threshold that is not a whole number of MB ----------------------------
# Silently ignoring one would be worse than failing: the switch would report
# health against a cap nobody applied.
expect "a fractional cap fails loudly" none "OTTER_DISK_MAX_DB_MB=1.5 is not a whole number of MB" \
	"OTTER_DISK_MAX_DB_MB=1.5"
expect "a cap with a unit suffix fails loudly" none "OTTER_DISK_MAX_RELEASES_MB=1GB is not a whole number of MB" \
	"OTTER_DISK_MAX_RELEASES_MB=1GB"
expect "a negative floor fails loudly" none "OTTER_DISK_MIN_FREE_MB=-1 is not a whole number of MB" \
	"OTTER_DISK_MIN_FREE_MB=-1"
expect "a cap with a trailing space fails loudly" none "OTTER_DISK_MAX_CACHE_MB=2  is not a whole number of MB" \
	"OTTER_DISK_MAX_CACHE_MB=2 "

# --- the metric endpoint and the data directory ------------------------------
# An endpoint nothing answers must be fatal: a check that cannot publish has not
# reported health, however healthy the stores are.
expect "a metric endpoint that cannot be reached fails loudly" none "PutMetricData" \
	"OTTER_METRIC_ENDPOINT=$DEAD_URL"
expect "a data directory that does not exist is a measurement failure" none "does-not-exist" \
	"OTTER_DATA_DIR=$work/does-not-exist"
expect "a data directory that is a file is a measurement failure" none "is not a directory" \
	"OTTER_DATA_DIR=$at_file"
# The other side of that line: the runtime creates the four stores lazily, so a
# data directory holding only otter.db is a fresh host, not a host in trouble.
fresh=$work/fresh
mkdir -p "$fresh"
dd if=/dev/zero of="$fresh/otter.db" bs=1024 count=1 2>/dev/null
expect "stores that do not exist yet measure 0MB and still publish" one "" "OTTER_DATA_DIR=$fresh"

# --- outcome -----------------------------------------------------------------
if [ "$failed" -ne 0 ]; then
	echo "test-disk-check: FAILED: $failed case(s), $passed passed" >&2
	exit 1
fi

echo "test-disk-check: all $passed cases passed"
