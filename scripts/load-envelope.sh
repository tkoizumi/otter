#!/bin/sh
# WS4 — measure the supported-load envelope on a reference-shaped host.
#
# The roadmap's exit evidence is a measurement, and the reference host is
# currently sized from a budget. This runs a committed, reproducible workload
# against a real runtime from this checkout and prints the numbers that
# docs/supported-load.md publishes.
#
# This is deliberately NOT a pass/fail drill: an envelope is a set of
# observations, and a number worse than the current budget is a result, not a
# failure. Nothing here asserts a threshold. Do not add one -- the moment this
# can "fail", someone tunes the workload until it passes.
#
#   make build && scripts/load-envelope.sh
#
# Environment:
#   ENVELOPE_RUNS      runs to submit (default 30)
#   ENVELOPE_JOB_SECS  seconds each run spends "on the wire" (default 0.6)
#   ENVELOPE_DEPTH     backlog depth for the latency reading (default 10)
#   ENVELOPE_KEEP=1    keep the workspace for inspection
#   ENVELOPE_JSON=1    emit JSON instead of the table
#
# A run on hardware that is not the reference shape is a *development
# observation*, not an envelope. The script detects the shape and says which it
# produced, because docs/supported-load.md may only publish reference figures.
#
# Never run this against the Castor production runtime: it holds three real
# jobs. This builds its own workspace under a temporary directory.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OTTER_BIN=${OTTER_BIN:-$root/bin/otter}

RUNS=${ENVELOPE_RUNS:-30}
JOB_SECS=${ENVELOPE_JOB_SECS:-0.6}
DEPTH=${ENVELOPE_DEPTH:-10}

[ -x "$OTTER_BIN" ] || { echo "load-envelope: not executable: $OTTER_BIN (run make build)" >&2; exit 2; }

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-envelope.XXXXXX")
cleanup() {
	status=$?
	( cd "$work" 2>/dev/null && "$OTTER_BIN" stop >/dev/null 2>&1 ) || true
	if [ "${ENVELOPE_KEEP:-0}" = "1" ]; then
		echo "load-envelope: workspace kept at $work" >&2
	else
		rm -rf "$work"
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM

# --- hardware, because every published figure must name it --------------------

cpu_model=$(awk -F': ' '/^model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null || echo unknown)
cores=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 0)
mem_mib=$(awk '/^MemTotal:/{printf "%d", $2/1024}' /proc/meminfo 2>/dev/null || echo 0)
arch=$(uname -m)
kernel=$(uname -sr)
os_name=$( (. /etc/os-release 2>/dev/null && printf '%s' "$PRETTY_NAME") || echo unknown )

# The reference shape is a t4g.micro: 2 vCPU, ~1 GiB, arm64, 20 GiB gp3. Strict
# on purpose -- a near miss is still an observation, and labelling one as the
# envelope is the error this check exists to prevent.
is_reference=no
if [ "$arch" = "aarch64" ] && [ "$cores" -ge 2 ] && [ "$mem_mib" -ge 900 ] && [ "$mem_mib" -le 1200 ]; then
	is_reference=yes
fi

# --- the committed workload ---------------------------------------------------
#
# One shape, because the envelope is about the runtime's overhead rather than a
# customer's code: bounded paginated work, no local CPU, concurrency 1. The
# sleeps stand in for remote calls and dominate the run, so the measurement
# describes scheduling, memory and storage rather than this script.

cd "$work"
"$OTTER_BIN" init envelope >/dev/null
jobdir=$work/envelope
cat >"$jobdir/main.py" <<PY
import time

def main(ctx):
    for _ in range(3):
        time.sleep($JOB_SECS / 3)
    ctx.log("measured run complete")
PY

"$OTTER_BIN" release envelope >/dev/null
"$OTTER_BIN" start --detach >/dev/null

i=0
until "$OTTER_BIN" status >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -lt 100 ] || { echo "load-envelope: daemon did not become ready" >&2; exit 1; }
	sleep 0.1
done

data=$work/.otter/data
base=$(cat "$work/.otter/serve/listen.url" 2>/dev/null || echo "")

# The daemon's accepted size is the database *plus* its write-ahead log: SQLite
# writes there first and only folds it back on checkpoint, so a .db that looks
# unchanged across a hundred runs is not a hundred runs of zero growth.
db_bytes() {
	db=$(wc -c <"$data/otter.db" 2>/dev/null || echo 0)
	wal=$(wc -c <"$data/otter.db-wal" 2>/dev/null || echo 0)
	echo $((db + wal))
}

# Peak RSS of the daemon and everything in its process group, sampled while work
# is in flight: the high-water mark is what sizes a host, not the idle value.
# Read from /proc because it is present everywhere this runs and `ps` output
# varies enough between images to break a strict shell.
rss_of_group() {
	python3 - "$work" <<'PY' 2>/dev/null || echo 0
import os, sys
work = sys.argv[1]
target_pgid = None
for pid in os.listdir('/proc'):
    if not pid.isdigit():
        continue
    try:
        with open(f'/proc/{pid}/cmdline', 'rb') as fh:
            cmd = fh.read().decode('utf-8', 'replace')
    except OSError:
        continue
    # The daemon is not called `otterd`: `otter start` runs the server itself,
    # so it appears as `<...>/bin/otter start --jobs <work> --data <work>/...`.
    # Match on the workspace path it was pointed at rather than a binary name,
    # which is what actually identifies our daemon rather than any other.
    if 'otter' in cmd and work in cmd:
        try:
            with open(f'/proc/{pid}/stat') as fh:
                rest = fh.read().rsplit(')', 1)[1].split()
            target_pgid = rest[2]
        except (OSError, IndexError):
            pass
        break
if target_pgid is None:
    print(0)
    sys.exit(0)

total_kib = 0
page_kib = os.sysconf('SC_PAGE_SIZE') // 1024
for pid in os.listdir('/proc'):
    if not pid.isdigit():
        continue
    try:
        with open(f'/proc/{pid}/stat') as fh:
            rest = fh.read().rsplit(')', 1)[1].split()
        if rest[2] != target_pgid:
            continue
        with open(f'/proc/{pid}/statm') as fh:
            rss_pages = int(fh.read().split()[1])
        total_kib += rss_pages * page_kib
    except (OSError, IndexError, ValueError):
        continue
print(total_kib)
PY
}

rss_peak_kb=0
sample_rss() {
	rss=$(rss_of_group)
	if [ "${rss:-0}" -gt "$rss_peak_kb" ]; then
		rss_peak_kb=$rss
	fi
	return 0
}

health_field() {
	curl -fsS "$base/health" 2>/dev/null | python3 -c "import json,sys;print(json.load(sys.stdin)$1)" 2>/dev/null || echo ""
}

# --- sustained run rate -------------------------------------------------------
#
# Submit through the API rather than `otter run`, which blocks until the run
# finishes. The API returns as soon as the run is queued, so a backlog exists to
# drain and the figure is throughput rather than the speed of a synchronous CLI.

db_before=$(db_bytes)
started=$(date +%s)
i=0
while [ "$i" -lt "$RUNS" ]; do
	curl -fsS -X POST "$base/v1/jobs/envelope/runs" >/dev/null 2>&1 || true
	sample_rss
	i=$((i + 1))
done

drained=no
i=0
while [ "$i" -lt 3000 ]; do
	queued=$(health_field ".get('queue_depth',0)")
	if [ "$queued" = "0" ]; then
		drained=yes
		break
	fi
	sample_rss
	i=$((i + 1))
	sleep 0.1
done
elapsed=$(( $(date +%s) - started ))
[ "$elapsed" -gt 0 ] || elapsed=1
db_after=$(db_bytes)
db_growth=$((db_after - db_before))
per_run=$((db_growth / RUNS))

# --- queue latency against depth ----------------------------------------------
#
# Fill a backlog and read the age the daemon reports for the oldest claimable
# run, so an operator can turn a depth into a wait. Sampled immediately after
# submission, before the pool catches up; once it drains the age is meaningless.

i=0
while [ "$i" -lt "$DEPTH" ]; do
	curl -fsS -X POST "$base/v1/jobs/envelope/runs" >/dev/null 2>&1 || true
	i=$((i + 1))
done
depth_now=$(health_field ".get('queue_depth',0)")
oldest=$(health_field ".get('queue',{}).get('oldest_waiting_seconds','')")

i=0
while [ "$i" -lt 3000 ]; do
	[ "$(health_field ".get('queue_depth',0)")" = "0" ] && break
	i=$((i + 1))
	sleep 0.1
done

rate=$(awk -v r="$RUNS" -v e="$elapsed" 'BEGIN{printf "%.2f", r/e}')

# --- report -------------------------------------------------------------------

if [ "${ENVELOPE_JSON:-0}" = "1" ]; then
	python3 - "$is_reference" "$arch" "$cpu_model" "$cores" "$mem_mib" "$os_name" "$kernel" \
		"$RUNS" "$JOB_SECS" "$elapsed" "$rate" "$drained" "$rss_peak_kb" \
		"$db_growth" "$per_run" "$DEPTH" "$depth_now" "$oldest" <<'PY'
import json, sys
(ref, arch, cpu, cores, mem, osname, kern, runs, secs, elapsed, rate, drained,
 rss, growth, per_run, depth, depth_now, oldest) = sys.argv[1:19]
print(json.dumps({
    "reference_host": ref == "yes",
    "hardware": {"arch": arch, "cpu": cpu, "cores": int(cores), "mem_mib": int(mem),
                 "os": osname, "kernel": kern},
    "workload": {"runs": int(runs), "job_seconds": float(secs), "concurrency": 1},
    "sustained": {"elapsed_seconds": int(elapsed), "runs_per_second": float(rate),
                  "drained": drained == "yes"},
    "memory": {"peak_rss_kib": int(rss)},
    "storage": {"db_plus_wal_growth_bytes": int(growth), "bytes_per_run": int(per_run)},
    "latency": {"target_depth": int(depth), "observed_depth": int(depth_now or 0),
                "oldest_waiting_seconds": float(oldest) if oldest else None},
}, indent=2))
PY
	exit 0
fi

cat <<REPORT
load-envelope: WS4 supported-load measurement

host
  reference shape (t4g.micro, 2 vCPU, ~1 GiB, arm64): $is_reference
  $cpu_model, $cores cores, ${mem_mib} MiB
  $arch, $os_name ($kernel)

workload (committed in scripts/load-envelope.sh)
  paginated job, concurrency 1, ~${JOB_SECS}s per run

measured
  sustained run rate      ${rate}/s  ($RUNS runs in ${elapsed}s, drained=$drained)
  peak daemon+child RSS   $((rss_peak_kb / 1024)) MiB
  otter.db + WAL growth   ${db_growth} bytes over $RUNS runs (${per_run} bytes/run)
  queue latency           depth ${depth_now} -> oldest waiting ${oldest:-n/a}s (target depth $DEPTH)

WHAT THESE NUMBERS ARE
REPORT

if [ "$is_reference" = "yes" ]; then
	echo "  A reference-shaped host, so these may be published as the envelope."
else
	cat <<'NOTE'
  A DEVELOPMENT OBSERVATION, not the envelope. This host is not the reference
  shape, so these figures describe this machine. Record them as an observation
  and leave the envelope tables in docs/supported-load.md empty until the same
  workload has been run on a t4g.micro.
NOTE
fi
