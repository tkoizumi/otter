#!/bin/sh
# WS5 overload — a job at its max_queue_depth refuses autonomous triggers, counts
# the refusal, and still admits a manual run.
#
# The claim (v0.5.0 WS3/WS5, runtime-contract.md §2): when a job's accepted-and-
# unfinished run count reaches max_queue_depth, a cron or webhook trigger is
# refused with 429 overloaded rather than queued, the refusal is counted and
# timestamped so /health and `otter status` can report it, and a manual run is
# still admitted because an operator asking for one run is not the backlog the
# bound exists to cap.
#
# It runs a real otterd from this checkout on a loopback port in its own
# workspace, so nothing here touches a deployed runtime. The concurrency is
# deliberately 1 and the job deliberately slow: the bound is only reachable when
# the queue can actually grow, which is the whole point of the drill.
#
# Falsifiability (DRILL_SABOTAGE=no-bound): omit max_queue_depth from the
# manifest, the shape every job had before this feature existed. The drill must
# go red on the FIRST assertion -- with no bound, autonomous triggers queue
# instead of being refused, so no refusal is ever counted. If that run passes,
# the drill is only testing that a daemon can run a cron job, not that the bound
# does anything.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
OTTERD_BIN=${OTTERD_BIN:-$root/bin/otterd}
OTTER_BIN=${OTTER_BIN:-$root/bin/otter}

SABOTAGE=${DRILL_SABOTAGE:-}
case "$SABOTAGE" in
"" | no-bound) ;;
*) echo "overload: unknown DRILL_SABOTAGE=$SABOTAGE (known: no-bound)" >&2; exit 2 ;;
esac

[ -x "$OTTERD_BIN" ] || { echo "overload: not executable: $OTTERD_BIN (run make build)" >&2; exit 2; }
[ -x "$OTTER_BIN" ] || { echo "overload: not executable: $OTTER_BIN (run make build)" >&2; exit 2; }

# A stray value in the caller's environment must not change what is measured.
unset OTTER_API_URL OTTER_API_TOKEN OTTER_WORKERS OTTER_DATA_DIR OTTER_JOBS_DIR \
	OTTER_LISTEN OTTER_CAPTURE_DEFAULT 2>/dev/null || true

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-overload.XXXXXX")
daemon_pid=""
cleaning=0

cleanup() {
	status=$?
	[ "$cleaning" -eq 1 ] && return
	cleaning=1
	[ -n "$daemon_pid" ] && kill "$daemon_pid" 2>/dev/null || true
	[ -n "$daemon_pid" ] && wait "$daemon_pid" 2>/dev/null || true
	if [ "$status" -ne 0 ] && [ -z "${CI:-}" ]; then
		echo "overload: failed; daemon log tail:" >&2
		tail -n 20 "$work/daemon.log" >&2 2>/dev/null || true
		echo "overload: workspace kept at $work" >&2
		return
	fi
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

fail() {
	echo "overload: FAILED: $1" >&2
	exit 1
}

free_port() {
	python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

BOUND=2
port=$(free_port)
base="http://127.0.0.1:$port"

# The workspace is built by the CLI rather than assembled by hand, because a run
# executes the *active release* and not the source tree: a job with no release is
# refused before any bound is consulted, which is exactly how this drill
# misreported itself the first time it was run.
ws="$work/ws"
mkdir -p "$ws"
(
	cd "$ws"
	"$OTTER_BIN" init slow >/dev/null
)
jobdir="$ws/slow"
[ -d "$jobdir" ] || fail "otter init did not create $jobdir"
data_dir="$ws/.otter/data"

# The bound is what this drill is about, so the sabotage removes exactly it and
# nothing else.
bound_line="max_queue_depth: $BOUND"
[ "$SABOTAGE" = "no-bound" ] && bound_line="# max_queue_depth omitted by DRILL_SABOTAGE=no-bound"

cat >"$jobdir/otter.yaml" <<YAML
version: 1
name: slow
entrypoint: main.py
timeout: 300
concurrency: 1
retry:
  attempts: 0
$bound_line
YAML

# Slow enough that the queue can grow while the drill submits, and a cron tick
# every second so the autonomous path is exercised rather than simulated.
cat >"$jobdir/main.py" <<'PY'
import time

def main(ctx):
    # Long enough that a one-second schedule builds an unambiguous backlog: at
    # ~6s the queue hovers at 0-1 and the bound is never reached, which is how
    # this drill first misreported itself.
    time.sleep(30)
    ctx.log("slow run done")
PY

echo "overload: workspace  $work"
echo "overload: bound      $bound_line"
echo "overload: schedule   @every 1s, concurrency 1, ~30s per run"

# Release before starting: a cron trigger for a job with no active release is
# rejected for a different reason, and the drill would fail for the wrong one.
# This is also why the drill builds a workspace rather than hand-assembling a
# jobs directory: a run executes the active release, not the source tree.
(
	cd "$ws"
	"$OTTER_BIN" release slow >/dev/null
)

OTTER_DATA_DIR="$data_dir" \
OTTER_JOBS_DIR="$ws" \
OTTER_LISTEN="127.0.0.1:$port" \
OTTER_WORKERS=1 \
OTTER_API_TOKEN=overload-token \
OTTER_CAPTURE_DEFAULT=off \
"$OTTERD_BIN" >"$work/daemon.log" 2>&1 &
daemon_pid=$!

api() { curl -fsS -H 'Authorization: Bearer overload-token' "$@"; }

ready=no
i=0
while [ "$i" -lt 100 ]; do
	if api "$base/health" >/dev/null 2>&1; then
		ready=yes
		break
	fi
	i=$((i + 1))
	sleep 0.1
done
[ "$ready" = "yes" ] || fail "the daemon never answered /health (see $work/daemon.log)"

# A cron trigger is the autonomous path. Arming it through the API means the
# drill does not depend on a manifest-owned schedule reconciling on reload.
job_id=$(api "$base/v1/jobs" | python3 -c '
import json, sys
jobs = json.load(sys.stdin)
items = jobs.get("jobs", jobs) if isinstance(jobs, dict) else jobs
for j in items:
    if j.get("name") == "slow":
        print(j.get("id", ""))
        break
')
[ -n "$job_id" ] || fail "the slow job was not discovered"

echo "overload: job id     $job_id"

api -X POST "$base/v1/jobs/$job_id/schedules" \
	-H 'Content-Type: application/json' \
	-d '{"cron":"@every 1s","timezone":"UTC"}' >/dev/null \
	|| fail "could not arm a cron schedule"

# Let the schedule build a backlog past the bound. Thirty seconds per run with a
# one-second tick means the queue passes the bound within a few seconds and
# stays there, so the sampling below cannot miss it.
sleep 12

refused_of() {
	api "$base/health" | python3 -c "
import json, sys
q = json.load(sys.stdin).get('queue', {})
print(q.get('$1', 0) or 0)
"
}

total=$(refused_of refused_total)
echo "overload: refused_total after the backlog built: $total"

# 1. The bound refused at least one autonomous trigger. Under no-bound sabotage
#    this is zero and the drill goes red here, which is the assertion that makes
#    it falsifiable.
[ "${total:-0}" -ge 1 ] || fail "no autonomous trigger was refused: the bound did not hold (refused_total=$total)"

# 2. The refusal is timestamped and attributed to the job, so it is reportable
#    rather than only counted.
api "$base/health" >"$work/health.json"
python3 - "$work/health.json" "$job_id" <<'PY' || fail "the refusal is not timestamped and attributed per job"
import json, sys
health = json.load(open(sys.argv[1]))
job_id = sys.argv[2]
q = health.get("queue", {})
if not q.get("last_refused_at"):
    print("overload: queue.last_refused_at is absent", file=sys.stderr)
    raise SystemExit(1)
by_job = q.get("refusals_by_job") or {}
entry = by_job.get(job_id)
if not entry or not entry.get("refused_total"):
    print(f"overload: no per-job refusal for {job_id}: {by_job}", file=sys.stderr)
    raise SystemExit(1)
if not entry.get("last_refused_at"):
    print("overload: the per-job refusal has no timestamp", file=sys.stderr)
    raise SystemExit(1)
PY
echo "overload: refusal is timestamped and attributed to $job_id"

# 3. A manual run is still admitted at the bound. This is the half that keeps
#    the bound from taking away the operator's own tool when the queue is deep.
manual_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$base/v1/jobs/$job_id/runs" \
	-H 'Authorization: Bearer overload-token')
[ "$manual_status" = "202" ] || fail "a manual run at the bound answered $manual_status, want 202"
echo "overload: manual run admitted at the bound (202)"

# 4. The journal records the refusal, so an operator who greps a log finds it.
grep -q 'admission_refused' "$work/daemon.log" || fail "the daemon journal has no admission_refused line"
echo "overload: daemon journal carries admission_refused"

echo
echo "overload: PASSED"
echo "  refused_total          $total"
echo "  a manual run           admitted (202) at the bound"
echo "  journal                admission_refused present"
exit 0
