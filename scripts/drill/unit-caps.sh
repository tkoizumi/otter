#!/bin/sh
# P0-04 unit caps — kill a deliberately runaway Python job with the caps in the
# generated systemd unit and observe what happens to otterd and the host.
#
# The unit is not a hand-written copy: scripts/drill/unit-caps-gen renders it
# with the same internal/deploy.UnitFile the deployer uses, so this drill tests
# what `otter deploy` would actually install.
#
# Every case runs with a real swap device present. The Castor host this claim
# failed on has a 2 GiB swapfile, and the old drill's swap-less container was
# one of the two differences that hid the defect; a swap-bound assertion is
# vacuous without a swap device to bound. Three cases run by default:
#
#   hard-cap         memory-max=192M memory-high=off  memory-swap-max=0
#       The hard cap as the only memory limiter, with the deployed swap default
#       (the cgroup may not swap). The runaway must be OOM-killed quickly.
#   swap-size        memory-max=192M memory-high=off  memory-swap-max=64M
#       The same with a small nonzero swap bound, which must still bind.
#   deployed-style   memory-max=25%  memory-high=20%  memory-swap-max=0
#       The deployed shape: systemd resolves the percentages, the soft cap is
#       ON, and the unit may not swap. This is the case the Castor failure was
#       found in and the one the old drill never ran. See the note below.
#
# Each case asserts all four clauses of the claim: the runaway is OOM-killed,
# no process but otterd is left in the unit's cgroup, otterd is not restarted,
# and /health answers within 5s. A case reports every clause it fails, not just
# the first, so a configuration that cannot kill the job still shows whether the
# daemon and the host survived it.
#
# FINDING (2026-09-30, kernel 5.10.76-linuxkit, cgroup v2): with MemoryHigh ON
# as deployed, the swap bound alone does not get the runaway killed in any
# useful time. memory.high throttles the allocator just above the soft cap,
# memory.swap.max=0 leaves reclaim nowhere to move it, and the process is pinned
# below memory.max. Measured on the deployed-style case: memory.current
# 842,346,496 B (803 MiB) at 20s and 871,403,520 B (831 MiB) at 590s, against
# memory.high 824,872,960 B (787 MiB) and memory.max 1,031,090,176 B (983 MiB),
# with memory.swap.current 0 and oom_kill 0 -- until the job's own manifest
# timeout (600s) killed it. The deployed-style case is therefore RED, and
# deliberately so: it is the executable record that MemorySwapMax=0 contains the
# blast radius (the cgroup never swaps, the host stays responsive, otterd is
# never restarted) but does not by itself satisfy the "killed" clause while
# MemoryHigh=60% is in force.
#
# The drill builds scripts/drill/unit-caps.Dockerfile (systemd in a container),
# so it does not depend on a locally cached image. On a machine that cannot run
# a privileged systemd container it exits non-zero and says what is required —
# a drill that did not run has produced no evidence.
#
#   sh scripts/drill.sh unit-caps
#
# Run one ad-hoc configuration instead of the three cases with:
#   MEMORY_MAX=256M MEMORY_HIGH=200M MEMORY_SWAP_MAX=0 sh scripts/drill.sh unit-caps
# and bound the observation window with KILL_WINDOW (seconds, default 180).
#
# Or override the image, the container's swap device, or the other caps with
# OTTER_SYSTEMD_IMAGE, SWAP_MB, CPU_QUOTA and TASKS_MAX.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

case "${GOARCH:-$(uname -m)}" in
arm64 | aarch64) goarch=arm64 ;;
amd64 | x86_64) goarch=amd64 ;;
*)
	echo "unit-caps: FAIL: cannot map $(uname -m) to a Go architecture; set GOARCH=amd64 or GOARCH=arm64" >&2
	exit 1
	;;
esac

if ! command -v docker >/dev/null 2>&1; then
	echo "unit-caps: FAIL: docker is required to run systemd in a privileged container;" >&2
	echo "unit-caps: there is no way to observe systemd cgroup caps on this machine without it." >&2
	exit 1
fi
if ! docker info >/dev/null 2>&1; then
	echo "unit-caps: FAIL: the docker daemon is not reachable" >&2
	exit 1
fi

image=${OTTER_SYSTEMD_IMAGE:-otter-systemd-caps}
name=otter-unit-caps-$$
work=$(mktemp -d "${TMPDIR:-/tmp}/otter-unit-caps.XXXXXX")
cpu_quota=${CPU_QUOTA:-200%}
tasks_max=${TASKS_MAX:-512}
swap_mb=${SWAP_MB:-512}
container=""

cleanup() {
	status=$?
	[ -n "$container" ] && docker rm -f "$container" >/dev/null 2>&1 || true
	if [ "$status" -ne 0 ]; then
		echo "unit-caps: FAILED (exit $status); work directory kept at $work" >&2
	else
		rm -rf "$work"
	fi
}
trap cleanup EXIT INT TERM

echo "unit-caps: image      $image"
echo "unit-caps: target     linux/$goarch"
echo "unit-caps: swap       ${swap_mb}M loop device inside the container"
echo "unit-caps: common caps cpu-quota=$cpu_quota tasks-max=$tasks_max"

echo
echo "unit-caps: building the systemd image"
docker build -q -t "$image" -f "$here/unit-caps.Dockerfile" "$here" >/dev/null

echo "unit-caps: building the linux binaries"
mkdir -p "$work/bin"
(
	cd "$root"
	GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 scripts/go build -o "$work/bin/otterd" ./cmd/otterd
	GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 scripts/go build -o "$work/bin/otter" ./cmd/otter
)

# run_case boots a fresh systemd container for one cap configuration, installs
# the unit the deployer renders for it, and runs the assertions inside.
run_case() {
	case_name=$1
	memory_max=$2
	memory_high=$3
	memory_swap_max=$4
	kill_window=${5:-180}
	case_name=$(printf '%s' "$case_name" | tr -c 'a-zA-Z0-9._-' '-')
	container="$name-$case_name"

	echo
	echo "================ unit-caps: case $case_name ================"
	echo "unit-caps: caps       memory-max=$memory_max memory-high=$memory_high memory-swap-max=$memory_swap_max"
	echo "unit-caps: observe    the OOM kill for up to ${kill_window}s"

	echo "unit-caps: rendering the unit the deployer would write"
	(
		cd "$root"
		scripts/go run ./scripts/drill/unit-caps-gen \
			-remote-dir /opt/otter -workspace drill -service otterd-drill -user otter \
			-listen 127.0.0.1:7337 \
			-memory-max "$memory_max" -memory-high "$memory_high" \
			-memory-swap-max "$memory_swap_max" \
			-cpu-quota "$cpu_quota" -tasks-max "$tasks_max"
	) >"$work/unit-$case_name.service"
	cat "$work/unit-$case_name.service"

	echo "unit-caps: starting the systemd container"
	docker run -d --name "$container" --privileged --cgroupns=host \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
		-v "$work/bin:/hostbin:ro" \
		-v "$work/unit-$case_name.service:/hostunit/unit.service:ro" \
		"$image" >/dev/null

	i=0
	while :; do
		state=$(docker exec "$container" systemctl is-system-running 2>/dev/null || true)
		case "$state" in
		running | degraded) break ;;
		esac
		i=$((i + 1))
		if [ "$i" -ge 60 ]; then
			echo "unit-caps: FAIL: systemd never reached running inside the container." >&2
			echo "unit-caps: this drill needs Docker able to run systemd as PID 1: --privileged," >&2
			echo "unit-caps: --cgroupns=host, and a writable /sys/fs/cgroup (cgroup v2)." >&2
			docker exec "$container" journalctl -b --no-pager 2>/dev/null | tail -40 >&2 || true
			exit 1
		fi
		sleep 1
	done
	echo "unit-caps: systemd is $state"

	docker exec -i \
		-e CASE_NAME="$case_name" \
		-e CASE_MEMORY_SWAP_MAX="$memory_swap_max" \
		-e CASE_KILL_WINDOW="$kill_window" \
		-e SWAP_MB="$swap_mb" \
		"$container" bash -s <<'INNER'
set -eu

SERVICE=otterd-drill
WS=/opt/otter/workspaces/drill
DATA="$WS/.otter/data"
JOB="$WS/jobs/runaway"
TOKEN=drill-token-not-a-secret
# The service cgroup is under the container's own cgroup, which cgroupns=host
# leaves in the path: systemd reports it relative to the hierarchy root, so the
# mount point is prepended once the unit is running.
CG=""

# The claim has four clauses, and a case must show all of them rather than
# stopping at the first one: the kill, no surviving runaway, otterd not
# restarted, and the host still answering. A configuration that fails the kill
# clause is exactly the one whose other clauses are most worth seeing, so the
# failures are collected and reported together. Configuration assertions (is the
# cgroup capped at all?) still exit immediately: they check the unit, not the
# claim.
claim_failures=""
claim_fail() {
	claim_failures="$claim_failures $1"
	echo "unit-caps: FAIL: $1" >&2
}

echo
echo "===== inside the container: case $CASE_NAME ====="
echo "kernel:  $(uname -sr)"
echo "systemd: $(systemctl --version | head -1)"
echo "python:  $(python3 --version 2>&1)"

# The Castor host this failed on has a 2 GiB swapfile, and that swapfile is what
# let the runaway outgrow its hard cap. Give the container its own swap device
# as well: with no swap at all, "the cgroup's swap is bounded" cannot be
# observed and the unbounded-swap failure cannot be reproduced. A loop device is
# used because overlayfs refuses a swapfile directly (swapon: Invalid argument).
echo
echo "--- swap device, the host's swapfile reproduced ---"
dd if=/dev/zero of=/swapfile bs=1M count="$SWAP_MB" status=none
chmod 600 /swapfile
loop=$(losetup -f --show /swapfile)
mkswap "$loop" >/dev/null
if ! swapon "$loop"; then
	echo "unit-caps: FAIL: swapon $loop failed; the swap-bound assertion needs a swap device to bound." >&2
	exit 1
fi
# swapon is global to the kernel, not namespaced to the container: removing the
# container does not undo it, and the loop device keeps the backing file alive
# (and its disk allocated) until the VM reboots. Undo both on the way out.
cleanup_swap() {
	swapoff "$loop" >/dev/null 2>&1 || true
	losetup -d "$loop" >/dev/null 2>&1 || true
}
trap cleanup_swap EXIT INT TERM
cat /proc/swaps
free -m

# The deploy's own layout: the service account, the workspace tree and the
# 0700 data directory, with both binaries inside it.
id -u otter >/dev/null 2>&1 || useradd --system --home-dir /opt/otter --no-create-home --shell /usr/sbin/nologin otter
install -d -m 0755 /opt/otter /opt/otter/workspaces
install -d -m 0755 -o otter -g otter "$WS" "$WS/bin" "$WS/jobs"
install -d -m 0700 -o otter -g otter "$DATA"
install -d -m 0700 /etc/otter/workspaces
install -m 0755 -o otter -g otter /hostbin/otterd "$WS/bin/otterd"
install -m 0755 -o otter -g otter /hostbin/otter "$WS/bin/otter"

# The runaway job. External Python, no managed environment, so the drill needs
# no network: the allocation loop is the only thing that stops it.
mkdir -p "$JOB"
cat >"$JOB/otter.yaml" <<'YAML'
version: 1
name: runaway
entrypoint: main.py
timeout: 600
concurrency: 1
YAML
cat >"$JOB/main.py" <<'PY'
import time

# Hold 16 MiB chunks forever. Nothing in this script stops; the cgroup memory
# cap is the only thing that can, which is exactly the behavior under test.
chunks = []
print("runaway: allocating until the memory cap kills me", flush=True)
while True:
    chunks.append(bytearray(16 * 1024 * 1024))
    if len(chunks) % 4 == 0:
        print("runaway: %d MiB held" % (len(chunks) * 16), flush=True)
    time.sleep(0.02)
PY
chown -R otter:otter "$WS/jobs"

printf 'OTTER_API_TOKEN=%s\n' "$TOKEN" >/etc/otter/workspaces/drill.env
chmod 0600 /etc/otter/workspaces/drill.env

# Release the job, then install the generated unit and start it.
cd "$WS"
runuser -u otter -- "$WS/bin/otter" release --jobs "$WS/jobs" --data "$DATA" "$JOB"
install -m 0644 /hostunit/unit.service "/etc/systemd/system/$SERVICE.service"
systemctl daemon-reload
systemctl enable --now "$SERVICE"

echo
echo "unit-caps: waiting for the health endpoint"
i=0
while ! curl -fsS http://127.0.0.1:7337/health >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -ge 100 ]; then
		echo "unit-caps: FAIL: otterd never answered /health" >&2
		journalctl -u "$SERVICE" -n 40 --no-pager >&2 || true
		exit 1
	fi
	sleep 0.2
done
echo "health: $(curl -fsS http://127.0.0.1:7337/health)"

CG="/sys/fs/cgroup$(systemctl show -p ControlGroup --value "$SERVICE")"
if [ ! -d "$CG" ]; then
	echo "unit-caps: FAIL: the unit's cgroup was not found (ControlGroup=$(systemctl show -p ControlGroup --value "$SERVICE"))" >&2
	echo "unit-caps: the memory and CPU caps are cgroup controls; without a cgroup v2 memory controller they cannot be observed." >&2
	exit 1
fi
echo "cgroup: $(systemctl show -p ControlGroup --value "$SERVICE")"

pid_before=$(systemctl show -p MainPID --value "$SERVICE")

# The cap must actually be in force. A unit that sets MemoryMax but whose
# cgroup ends up at "max" -- an out-of-range value systemd quietly ignored, or a
# memory controller that was never delegated -- would otherwise only be caught
# by luck when the OOM kill failed to happen.
want_cap=$(sed -n 's/^MemoryMax=//p' /hostunit/unit.service | head -1)
got_cap=$(cat "$CG/memory.max")
case "$want_cap" in
"")
	[ "$got_cap" = "max" ] || {
		echo "unit-caps: FAIL: the unit sets no MemoryMax but the cgroup is capped at $got_cap" >&2
		exit 1
	}
	echo "memory.max:  $got_cap (unit sets no MemoryMax)"
	;;
*% | infinity)
	# systemd resolves a percentage against host RAM, so only report it.
	echo "memory.max:  $got_cap (from MemoryMax=$want_cap)"
	;;
*)
	want_bytes=$(numfmt --from=iec "$want_cap" 2>/dev/null) || {
		echo "unit-caps: FAIL: cannot convert MemoryMax=$want_cap to bytes; use an IEC size such as 192M" >&2
		exit 1
	}
	if [ "$got_cap" != "$want_bytes" ]; then
		echo "unit-caps: FAIL: the unit sets MemoryMax=$want_cap ($want_bytes bytes) but memory.max is $got_cap" >&2
		exit 1
	fi
	echo "memory.max:  $got_cap (set from MemoryMax=$want_cap)"
	;;
esac
echo "memory.high: $(cat "$CG/memory.high")"

# The swap bound must be in force too, and it is the one cap that makes
# MemoryMax binding on a host with a swapfile: an unset or ignored
# MemorySwapMax leaves the cgroup free to page a runaway out instead of letting
# it reach memory.max, so the OOM killer never runs. Check it against what the
# drill asked for, not only against the unit: a renderer that silently dropped
# the directive would otherwise pass by agreeing with itself.
want_swap=$CASE_MEMORY_SWAP_MAX
emitted_swap=$(sed -n 's/^MemorySwapMax=//p' /hostunit/unit.service | head -1)
got_swap=$(cat "$CG/memory.swap.max")
case "$want_swap" in
off | "")
	if [ -n "$emitted_swap" ]; then
		echo "unit-caps: FAIL: the drill opted out of the swap bound but the unit sets MemorySwapMax=$emitted_swap" >&2
		exit 1
	fi
	if [ "$got_swap" != "max" ]; then
		echo "unit-caps: FAIL: the unit sets no MemorySwapMax but memory.swap.max is $got_swap" >&2
		exit 1
	fi
	echo "memory.swap.max: $got_swap (unit sets no MemorySwapMax; the cgroup may swap without bound)"
	;;
*% | infinity)
	if [ "$emitted_swap" != "$want_swap" ]; then
		echo "unit-caps: FAIL: the drill requested MemorySwapMax=$want_swap but the unit has '$emitted_swap'" >&2
		exit 1
	fi
	# systemd resolves a percentage against host RAM, so only report it.
	echo "memory.swap.max: $got_swap (from MemorySwapMax=$want_swap)"
	;;
*)
	if [ "$emitted_swap" != "$want_swap" ]; then
		echo "unit-caps: FAIL: the drill requested MemorySwapMax=$want_swap but the unit has '$emitted_swap'" >&2
		exit 1
	fi
	want_swap_bytes=$(numfmt --from=iec "$emitted_swap" 2>/dev/null) || {
		echo "unit-caps: FAIL: cannot convert MemorySwapMax=$emitted_swap to bytes; use an IEC size such as 0 or 64M" >&2
		exit 1
	}
	if [ "$got_swap" != "$want_swap_bytes" ]; then
		echo "unit-caps: FAIL: the unit sets MemorySwapMax=$emitted_swap ($want_swap_bytes bytes) but memory.swap.max is $got_swap" >&2
		exit 1
	fi
	echo "memory.swap.max: $got_swap (set from MemorySwapMax=$emitted_swap)"
	;;
esac
echo "otterd MainPID before: $pid_before"

echo
echo "unit-caps: triggering the runaway job"
runuser -u otter -- env OTTER_API_URL=http://127.0.0.1:7337 OTTER_API_TOKEN="$TOKEN" \
	"$WS/bin/otter" run runaway

i=0
oom=0
while [ "$i" -lt $((CASE_KILL_WINDOW * 5)) ]; do
	oom=$(awk '/^oom_kill /{print $2}' "$CG/memory.events" 2>/dev/null || echo 0)
	[ "${oom:-0}" -ge 1 ] && break
	i=$((i + 1))
	sleep 0.2
done
observed=$((i / 5))

echo
echo "--- cgroup memory.events ---"
cat "$CG/memory.events"
echo "--- cgroup memory.peak (bytes) ---"
cat "$CG/memory.peak" 2>/dev/null || echo "(no memory.peak on this kernel)"
echo "--- cgroup memory.max (bytes) ---"
cat "$CG/memory.max"
echo "--- cgroup memory.swap.max (bytes) ---"
cat "$CG/memory.swap.max"
echo "--- cgroup memory.swap.current (bytes) ---"
cat "$CG/memory.swap.current" 2>/dev/null || echo "(no memory.swap.current on this kernel)"
echo "--- cgroup memory.current (bytes) ---"
cat "$CG/memory.current"

if [ "${oom:-0}" -lt 1 ]; then
	claim_fail "the runaway was NOT OOM-killed within ${CASE_KILL_WINDOW}s (oom_kill=${oom:-0} after ${observed}s; the hard cap never bound)"
	echo "--- daemon journal ---" >&2
	journalctl -u "$SERVICE" -n 60 --no-pager >&2 || true
fi

sleep 1
echo
echo "--- unit state after the ${CASE_KILL_WINDOW}s observation ---"
echo "is-active: $(systemctl is-active "$SERVICE")"
pid_after=$(systemctl show -p MainPID --value "$SERVICE")
nrestarts=$(systemctl show -p NRestarts --value "$SERVICE")
echo "otterd MainPID after:  $pid_after"
echo "NRestarts:             $nrestarts"
echo "OOMPolicy:             $(systemctl show -p OOMPolicy --value "$SERVICE")"

# "No runaway survived" has to be read from the cgroup's process list. The
# runtime starts the child as `python3 -m otter._launcher main.py` with the job
# directory as its cwd, so a pgrep on the job's path ('runaway/main.py') matches
# nothing even while the runaway is alive -- it reported clean on the Castor
# host while a 615 MiB python3 was still running. Anything but otterd itself
# left in the unit's cgroup is a survivor.
echo "--- processes left in the unit cgroup (want only otterd) ---"
echo "cgroup.procs: $(tr '\n' ' ' <"$CG/cgroup.procs")"
survivors=""
for pid in $(cat "$CG/cgroup.procs" 2>/dev/null); do
	[ "$pid" = "$pid_after" ] && continue
	comm=$(cat "/proc/$pid/comm" 2>/dev/null || echo "(gone)")
	cmdline=$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null || true)
	rss=$(awk '/^VmRSS:/{print $2" "$3}' "/proc/$pid/status" 2>/dev/null || true)
	echo "  survivor: pid $pid comm=$comm rss=${rss:-?} cmdline=${cmdline:-?}"
	survivors="$survivors $pid"
done
if [ -n "$survivors" ]; then
	claim_fail "a runaway process survived in the unit cgroup:$survivors"
else
	echo "  (none: only otterd pid $pid_after)"
fi

echo "--- run history ---"
runuser -u otter -- env OTTER_API_URL=http://127.0.0.1:7337 OTTER_API_TOKEN="$TOKEN" \
	"$WS/bin/otter" runs runaway --limit 3 || true

echo "--- host responsiveness ---"
host_state=$(systemctl is-system-running 2>/dev/null || true)
echo "is-system-running: $host_state"
case "$host_state" in
running | degraded) ;;
*)
	claim_fail "systemd is no longer running the host ($host_state)"
	;;
esac
# A bounded request to the daemon is the real responsiveness check: timeout
# fails if the host cannot schedule it, and curl fails if otterd is not serving.
if ! timeout 5 curl -fsS http://127.0.0.1:7337/health; then
	claim_fail "otterd did not answer /health within 5s"
fi
echo

echo "--- daemon journal, OOM lines ---"
journalctl -u "$SERVICE" --no-pager 2>/dev/null | grep -iE 'oom|out of memory|killed process' | tail -20 || true

# The evidence clause is that otterd survives, not merely that it is restarted.
if [ "$pid_before" != "$pid_after" ]; then
	claim_fail "otterd did not survive (MainPID $pid_before -> $pid_after)"
fi
if [ "$nrestarts" != "0" ]; then
	claim_fail "otterd restarted during the run (NRestarts=$nrestarts)"
fi
if [ "$(systemctl is-active "$SERVICE")" != "active" ]; then
	claim_fail "the unit is not active after the run"
fi

if [ -n "$claim_failures" ]; then
	echo "unit-caps: FAIL [$CASE_NAME]: unmet claim clauses:$claim_failures" >&2
	echo "unit-caps: the caps in force were: memory.max=$(cat "$CG/memory.max") memory.high=$(cat "$CG/memory.high") memory.swap.max=$(cat "$CG/memory.swap.max")" >&2
	exit 1
fi

echo "unit-caps: PASS [$CASE_NAME]: the runaway job was OOM-killed (oom_kill=$(awk '/^oom_kill /{print $2}' "$CG/memory.events")), no runaway process survived, otterd (PID $pid_after) survived, and the host answered."
INNER

	docker rm -f "$container" >/dev/null 2>&1 || true
	container=""
}

if [ -n "${MEMORY_MAX:-}${MEMORY_HIGH:-}${MEMORY_SWAP_MAX:-}" ]; then
	run_case custom "${MEMORY_MAX:-192M}" "${MEMORY_HIGH:-off}" "${MEMORY_SWAP_MAX:-0}" "${KILL_WINDOW:-180}"
else
	# The hard cap as the only memory limiter, with the deployed swap default
	# (0 = the cgroup may not swap). The runaway must be OOM-killed here, fast:
	# this is the case that shows memory.max binds at all.
	run_case hard-cap 192M off 0 60
	# The same, with a small nonzero swap bound: bounded swap must still let the
	# hard cap bind, and this exercises the drill's size conversion.
	run_case swap-size 192M off 64M 60
	# The deployed caps as they actually ship: percentages systemd resolves and
	# MemoryHigh ON. This is the case that was missing, and the one the Castor
	# failure was found in.
	run_case deployed-style 25% 20% 0 180
fi

echo
echo "unit-caps: all cases PASS"
