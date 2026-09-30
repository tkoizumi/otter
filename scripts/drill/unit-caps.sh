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
# vacuous without a swap device to bound. Four cases run by default:
#
#   deployed-style   memory-max=25%  memory-high=DEFAULT  memory-swap-max=0
#       The deployed defaults as they now ship, and the one case that renders
#       them: -memory-high is not passed at all, so the generator falls back to
#       deploy.DefaultMemoryHigh and the unit is what a plain `otter deploy`
#       writes. It asserts no MemoryHigh directive and memory.high == max, so
#       putting a soft cap back into the default turns this case red. The
#       percentage is scaled down from 75% because this container shares a
#       3.9 GiB VM with the rest of the machine; the shape is what is under
#       test. The runaway must be OOM-killed, with no survivor.
#   hard-cap         memory-max=192M memory-high=off  memory-swap-max=0
#       The same shape with an absolute cap, which exercises the byte-for-byte
#       comparison of memory.max against an IEC size.
#   swap-size        memory-max=192M memory-high=off  memory-swap-max=64M
#       A small nonzero swap bound, which must still let the hard cap bind.
#   soft-cap         memory-max=192M memory-high=128M memory-swap-max=0
#       The soft cap is a real feature, and this case records its honest cost.
#       It asserts no OOM kill, memory.current below memory.max, no swap use,
#       no process left in the cgroup after the run ends, otterd un-restarted
#       and the host answering, and that the run ends at its (short, 30s)
#       manifest timeout rather than by the kernel. It is the executable reason
#       MemoryHigh defaults to off: with the swap bound in place the kernel
#       cannot reclaim the allocator's pages, so a hold-everything job parks
#       above memory.high and creeps toward memory.max.
#
# The three "kill" cases assert all four clauses of the claim: the runaway is
# OOM-killed, no process but otterd is left in the unit's cgroup, otterd is not
# restarted, and /health answers within 5s. A case reports every clause it fails,
# not just the first, so a configuration that cannot kill the job still shows
# whether the daemon and the host survived it.
#
# FINDING that set the default (2026-09-30, kernel 5.10.76-linuxkit, cgroup v2):
# with MemoryHigh ON, the swap bound alone does not get the runaway killed in
# any useful time. Measured with MemoryHigh=20%/MemoryMax=25% (the same ratio as
# the deployed 60%/75%): memory.current settled at 842,346,496 B (803 MiB) after
# 20s and 871,403,520 B (831 MiB) after 590s, against memory.high 824,872,960 B
# (787 MiB) and memory.max 1,031,090,176 B (983 MiB), with memory.swap.current 0
# and oom_kill 0 until the job's own 600s manifest timeout ended it. A one-second
# OOM kill became a ten-minute pin, and on a single-worker host that occupies the
# only worker. DefaultMemoryHigh is therefore "off"; --memory-high still works
# and the soft-cap case above documents what setting it costs.
#
# The drill builds scripts/drill/unit-caps.Dockerfile (systemd in a container),
# so it does not depend on a locally cached image. On a machine that cannot run
# a privileged systemd container it exits non-zero and says what is required —
# a drill that did not run has produced no evidence.
#
#   sh scripts/drill.sh unit-caps
#
# Run one ad-hoc configuration instead of the four cases with:
#   MEMORY_MAX=256M MEMORY_HIGH=200M MEMORY_SWAP_MAX=0 sh scripts/drill.sh unit-caps
# and bound the observation window with KILL_WINDOW (seconds, default 180), pick
# the expectation with EXPECT=kill|soft-cap, or set the job's manifest timeout
# with JOB_TIMEOUT (seconds). MEMORY_HIGH=default omits the flag and renders the
# deploy's own default, the same way the deployed-style case does.
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
	case_expect=${6:-kill}
	case_job_timeout=${7:-600}
	case_name=$(printf '%s' "$case_name" | tr -c 'a-zA-Z0-9._-' '-')
	container="$name-$case_name"

	echo
	echo "================ unit-caps: case $case_name ================"
	echo "unit-caps: caps       memory-max=$memory_max memory-high=$memory_high memory-swap-max=$memory_swap_max"
	echo "unit-caps: expect     $case_expect; job manifest timeout ${case_job_timeout}s"
	if [ "$case_expect" = soft-cap ]; then
		echo "unit-caps: observe    the soft cap for ${kill_window}s"
	else
		echo "unit-caps: observe    the OOM kill for up to ${kill_window}s"
	fi

	# "default" means the drill does not pass the flag at all, so the generator
	# uses deploy.DefaultMemoryHigh and the rendered unit is what a plain
	# `otter deploy` writes. That is the only way the drill exercises the
	# constant; every other case passes an explicit value.
	echo "unit-caps: rendering the unit the deployer would write"
	set -- -remote-dir /opt/otter -workspace drill -service otterd-drill -user otter \
		-listen 127.0.0.1:7337 -memory-max "$memory_max"
	if [ "$memory_high" != default ]; then
		set -- "$@" -memory-high "$memory_high"
	fi
	set -- "$@" -memory-swap-max "$memory_swap_max" -cpu-quota "$cpu_quota" -tasks-max "$tasks_max"
	(
		cd "$root"
		scripts/go run ./scripts/drill/unit-caps-gen "$@"
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
		-e CASE_MEMORY_HIGH="$memory_high" \
		-e CASE_MEMORY_SWAP_MAX="$memory_swap_max" \
		-e CASE_KILL_WINDOW="$kill_window" \
		-e CASE_EXPECT="$case_expect" \
		-e CASE_JOB_TIMEOUT="$case_job_timeout" \
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
# swapoff can fail transiently -- the device is shared with every process in the
# VM, and faulting their pages back in needs memory, which is scarce right after
# an OOM kill -- so retry. A device that still cannot be released fails the case:
# a leaked swap device is a real side effect, not a footnote.
cleanup_swap() {
	i=0
	while [ "$i" -lt 30 ]; do
		if swapoff "$loop" >/dev/null 2>&1; then
			losetup -d "$loop" >/dev/null 2>&1 || true
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	echo "unit-caps: FAIL: could not release swap device $loop after 30 attempts; run: swapoff $loop && losetup -d $loop" >&2
	exit 1
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
cat >"$JOB/otter.yaml" <<YAML
version: 1
name: runaway
entrypoint: main.py
timeout: $CASE_JOB_TIMEOUT
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

# The soft cap is the default this drill now protects. The deployed-style case
# omits -memory-high entirely, so the generator falls back to
# deploy.DefaultMemoryHigh and the rendered unit is what a plain `otter deploy`
# writes; if that constant ever becomes a value again, the directive appears
# here and the case fails before anything runs. "off" (or empty) must leave
# memory.high at max, and an explicit value must match what systemd resolved.
want_high=$CASE_MEMORY_HIGH
emitted_high=$(sed -n 's/^MemoryHigh=//p' /hostunit/unit.service | head -1)
got_high=$(cat "$CG/memory.high")
case "$want_high" in
default | off | "")
	if [ -n "$emitted_high" ]; then
		echo "unit-caps: FAIL: the default unit emits MemoryHigh=$emitted_high; a soft cap parks a runaway below memory.max instead of letting the OOM killer run." >&2
		exit 1
	fi
	if [ "$got_high" != "max" ]; then
		echo "unit-caps: FAIL: the unit sets no MemoryHigh but memory.high is $got_high" >&2
		exit 1
	fi
	echo "memory.high: max (the unit sets no MemoryHigh)"
	;;
*% | infinity)
	if [ "$emitted_high" != "$want_high" ]; then
		echo "unit-caps: FAIL: the drill requested MemoryHigh=$want_high but the unit has '$emitted_high'" >&2
		exit 1
	fi
	# systemd resolves a percentage against host RAM, so only report it.
	echo "memory.high: $got_high (from MemoryHigh=$want_high)"
	;;
*)
	if [ "$emitted_high" != "$want_high" ]; then
		echo "unit-caps: FAIL: the drill requested MemoryHigh=$want_high but the unit has '$emitted_high'" >&2
		exit 1
	fi
	want_high_bytes=$(numfmt --from=iec "$emitted_high" 2>/dev/null) || {
		echo "unit-caps: FAIL: cannot convert MemoryHigh=$emitted_high to bytes; use an IEC size such as 128M" >&2
		exit 1
	}
	if [ "$got_high" != "$want_high_bytes" ]; then
		echo "unit-caps: FAIL: the unit sets MemoryHigh=$emitted_high ($want_high_bytes bytes) but memory.high is $got_high" >&2
		exit 1
	fi
	echo "memory.high: $got_high (set from MemoryHigh=$emitted_high)"
	;;
esac

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
run_id=$(runuser -u otter -- env OTTER_API_URL=http://127.0.0.1:7337 OTTER_API_TOKEN="$TOKEN" \
	"$WS/bin/otter" run runaway | head -1 | tr -d '[:space:]')
echo "run id: $run_id"
if [ -z "$run_id" ]; then
	echo "unit-caps: FAIL: 'otter run runaway' printed no run id; cannot follow the run to its terminal state." >&2
	exit 1
fi

if [ "$CASE_EXPECT" = "soft-cap" ]; then
	# The soft cap is a real systemd feature, and this case records its honest
	# cost rather than hiding it. With MemorySwapMax=0 there is nowhere to
	# reclaim the allocator's anonymous pages to, so it parks just above
	# memory.high and creeps toward memory.max instead of being killed; the run
	# ends only at its own manifest timeout. Nothing here waits for an OOM kill,
	# because under this configuration there must not be one.
	soft_started=$(date +%s)
	sleep 10
	soft_current=$(cat "$CG/memory.current")
	soft_high=$(cat "$CG/memory.high")
	soft_max=$(cat "$CG/memory.max")
	soft_swap=$(cat "$CG/memory.swap.current")
	echo
	echo "--- soft-cap sample after 10s (bytes) ---"
	echo "memory.current      $soft_current"
	echo "memory.high         $soft_high"
	echo "memory.max          $soft_max"
	echo "memory.swap.current $soft_swap"
	echo "oom_kill            $(awk '/^oom_kill /{print $2}' "$CG/memory.events")"

	# Wait for the run to reach a terminal state. It must be ended by its own
	# manifest timeout, not by the OOM killer, so the status is timed_out.
	i=0
	run_status=""
	while [ "$i" -lt $((CASE_JOB_TIMEOUT + 60)) ]; do
		run_status=$(runuser -u otter -- env OTTER_API_URL=http://127.0.0.1:7337 OTTER_API_TOKEN="$TOKEN" \
			"$WS/bin/otter" runs runaway --limit 5 2>/dev/null | awk -v id="$run_id" '$1==id{print $4}')
		case "$run_status" in "" | running) ;; *) break ;; esac
		i=$((i + 1))
		sleep 1
	done
	soft_elapsed=$(($(date +%s) - soft_started))
	echo "run ended after ${soft_elapsed}s of wall clock with status '${run_status}' (manifest timeout ${CASE_JOB_TIMEOUT}s)"
	oom=$(awk '/^oom_kill /{print $2}' "$CG/memory.events" 2>/dev/null || echo 0)
	[ "${oom:-0}" = "0" ] || claim_fail "the soft cap OOM-killed the job (oom_kill=$oom); this case documents that MemoryHigh parks a runaway below memory.max instead"
	{ [ -n "$soft_high" ] && [ "$soft_high" != "max" ]; } || claim_fail "memory.high is '${soft_high}', so the case did not run with a soft cap"
	[ "$soft_current" -lt "$soft_max" ] || claim_fail "memory.current ($soft_current) reached memory.max ($soft_max) with MemoryHigh on"
	[ "$soft_swap" = "0" ] || claim_fail "the cgroup swapped ${soft_swap} bytes despite MemorySwapMax=0"
	[ "$run_status" = "timed_out" ] || claim_fail "the run ended as '${run_status}', not by its ${CASE_JOB_TIMEOUT}s manifest timeout"

	# The survivor clause applies to this case too, checked at the terminal
	# state: the run ended by timeout, so the runtime has reaped the job and
	# only otterd should be left in the cgroup. (The kill cases run the same
	# scan in the shared block below, guarded so it does not run twice.)
	echo "--- soft-cap processes left in the unit cgroup (want only otterd) ---"
	echo "cgroup.procs: $(tr '\n' ' ' <"$CG/cgroup.procs")"
	soft_pids=""
	for pid in $(cat "$CG/cgroup.procs" 2>/dev/null); do
		[ "$pid" = "$(systemctl show -p MainPID --value "$SERVICE")" ] && continue
		comm=$(cat "/proc/$pid/comm" 2>/dev/null || echo "(gone)")
		cmdline=$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null || true)
		echo "  survivor: pid $pid comm=$comm cmdline=${cmdline:-?}"
		soft_pids="$soft_pids $pid"
	done
	if [ -n "$soft_pids" ]; then
		claim_fail "a runaway process survived the soft-cap run in the unit cgroup:$soft_pids"
	else
		echo "  (none: only otterd)"
	fi
else
	i=0
	oom=0
	while [ "$i" -lt $((CASE_KILL_WINDOW * 5)) ]; do
		oom=$(awk '/^oom_kill /{print $2}' "$CG/memory.events" 2>/dev/null || echo 0)
		[ "${oom:-0}" -ge 1 ] && break
		i=$((i + 1))
		sleep 0.2
	done
	observed=$((i / 5))
	if [ "${oom:-0}" -lt 1 ]; then
		claim_fail "the runaway was NOT OOM-killed within ${CASE_KILL_WINDOW}s (oom_kill=${oom:-0} after ${observed}s; the hard cap never bound)"
		echo "--- daemon journal ---" >&2
		journalctl -u "$SERVICE" -n 60 --no-pager >&2 || true
	fi
fi

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

sleep 1
echo
echo "--- unit state after the observation ---"
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
#
# The soft-cap case runs this scan in its own branch (against the terminal
# state) and guards this one off, so the clause is never checked twice.
if [ "$CASE_EXPECT" != "soft-cap" ]; then
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

if [ "$CASE_EXPECT" = "soft-cap" ]; then
	echo "unit-caps: PASS [$CASE_NAME]: the soft cap parked the runaway below memory.max (oom_kill=0, memory.swap.current=0), no runaway process survived the timeout, otterd (PID $pid_after) was not restarted, the host answered, and the run ended at its ${CASE_JOB_TIMEOUT}s manifest timeout rather than by OOM."
else
	echo "unit-caps: PASS [$CASE_NAME]: the runaway job was OOM-killed (oom_kill=$(awk '/^oom_kill /{print $2}' "$CG/memory.events")), no runaway process survived, otterd (PID $pid_after) survived, and the host answered."
fi
INNER

	docker rm -f "$container" >/dev/null 2>&1 || true
	container=""
}

if [ -n "${MEMORY_MAX:-}${MEMORY_HIGH:-}${MEMORY_SWAP_MAX:-}" ]; then
	run_case custom "${MEMORY_MAX:-192M}" "${MEMORY_HIGH:-off}" "${MEMORY_SWAP_MAX:-0}" \
		"${KILL_WINDOW:-180}" "${EXPECT:-kill}" "${JOB_TIMEOUT:-600}"
else
	# The deployed defaults as they now ship, rendered with the flag omitted
	# ("default") so the generator falls back to deploy.DefaultMemoryHigh and
	# this case tests the unit a plain `otter deploy` writes -- not a
	# hand-picked value. The percentage is scaled down from the deployed 75%
	# because this container shares a 3.9 GiB VM with the rest of the machine;
	# the shape is what is under test. The runaway must be OOM-killed, with no
	# survivor, and memory.high must be max.
	run_case deployed-style 25% default 0 60 kill 600
	# The same shape with an absolute hard cap, which also exercises the
	# byte-for-byte comparison of memory.max against an IEC size.
	run_case hard-cap 192M off 0 60 kill 600
	# A small nonzero swap bound must still let the hard cap bind, and this
	# exercises the drill's size conversion.
	run_case swap-size 192M off 64M 60 kill 600
	# The soft cap is a real feature, and this case records its honest cost: the
	# runaway parks below memory.max, is never OOM-killed, and the run ends at
	# its (deliberately short, 30s) manifest timeout. See the header note.
	run_case soft-cap 192M 128M 0 60 soft-cap 30
fi

echo
echo "unit-caps: all cases PASS"
