#!/bin/sh
# P0-04 unit caps — kill a deliberately runaway Python job with the caps in the
# generated systemd unit and observe what happens to otterd and the host.
#
# The unit is not a hand-written copy: scripts/drill/unit-caps-gen renders it
# with the same internal/deploy.UnitFile the deployer uses, so this drill tests
# what `otter deploy` would actually install. The caps are overridden so the
# runaway is killed in seconds: a small 192 MiB MemoryMax, and MemoryHigh off so
# the hard cap is the binding constraint. With the soft cap on, the kernel
# throttles and reclaims first, which is correct behavior but can keep a
# hold-everything allocator pinned below the hard cap for a long time. The
# soft-cap default is covered by the render tests.
#
# The drill builds scripts/drill/unit-caps.Dockerfile (systemd in a container),
# so it does not depend on a locally cached image. On a machine that cannot run
# a privileged systemd container it exits non-zero and says what is required —
# a drill that did not run has produced no evidence.
#
#   sh scripts/drill.sh unit-caps
#
# Override the cap the drill exercises, or the image tag, with:
#   MEMORY_MAX=256M MEMORY_HIGH=200M sh scripts/drill.sh unit-caps
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
memory_max=${MEMORY_MAX:-192M}
memory_high=${MEMORY_HIGH:-off}
cpu_quota=${CPU_QUOTA:-200%}
tasks_max=${TASKS_MAX:-512}

cleanup() {
	status=$?
	docker rm -f "$name" >/dev/null 2>&1 || true
	if [ "$status" -ne 0 ]; then
		echo "unit-caps: FAILED (exit $status); work directory kept at $work" >&2
	else
		rm -rf "$work"
	fi
}
trap cleanup EXIT INT TERM

echo "unit-caps: image      $image"
echo "unit-caps: target     linux/$goarch"
echo "unit-caps: caps       memory-max=$memory_max memory-high=$memory_high cpu-quota=$cpu_quota tasks-max=$tasks_max"

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

echo "unit-caps: rendering the unit the deployer would write"
(
	cd "$root"
	scripts/go run ./scripts/drill/unit-caps-gen \
		-remote-dir /opt/otter -workspace drill -service otterd-drill -user otter \
		-listen 127.0.0.1:7337 \
		-memory-max "$memory_max" -memory-high "$memory_high" \
		-cpu-quota "$cpu_quota" -tasks-max "$tasks_max"
) >"$work/unit.service"
cat "$work/unit.service"

echo
echo "unit-caps: starting the systemd container"
docker run -d --name "$name" --privileged --cgroupns=host \
	-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
	-v "$work/bin:/hostbin:ro" \
	-v "$work/unit.service:/hostunit/unit.service:ro" \
	"$image" >/dev/null

i=0
while :; do
	state=$(docker exec "$name" systemctl is-system-running 2>/dev/null || true)
	case "$state" in
	running | degraded) break ;;
	esac
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		echo "unit-caps: FAIL: systemd never reached running inside the container." >&2
		echo "unit-caps: this drill needs Docker able to run systemd as PID 1: --privileged," >&2
		echo "unit-caps: --cgroupns=host, and a writable /sys/fs/cgroup (cgroup v2)." >&2
		docker exec "$name" journalctl -b --no-pager 2>/dev/null | tail -40 >&2 || true
		exit 1
	fi
	sleep 1
done
echo "unit-caps: systemd is $state"

docker exec -i "$name" bash -s <<'INNER'
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

echo
echo "===== inside the container ====="
echo "kernel:  $(uname -sr)"
echo "systemd: $(systemctl --version | head -1)"
echo "python:  $(python3 --version 2>&1)"

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
echo "otterd MainPID before: $pid_before"

echo
echo "unit-caps: triggering the runaway job"
runuser -u otter -- env OTTER_API_URL=http://127.0.0.1:7337 OTTER_API_TOKEN="$TOKEN" \
	"$WS/bin/otter" run runaway

i=0
oom=0
while [ "$i" -lt 300 ]; do
	oom=$(awk '/^oom_kill /{print $2}' "$CG/memory.events" 2>/dev/null || echo 0)
	[ "${oom:-0}" -ge 1 ] && break
	i=$((i + 1))
	sleep 0.2
done

echo
echo "--- cgroup memory.events ---"
cat "$CG/memory.events"
echo "--- cgroup memory.peak (bytes) ---"
cat "$CG/memory.peak" 2>/dev/null || echo "(no memory.peak on this kernel)"
echo "--- cgroup memory.max (bytes) ---"
cat "$CG/memory.max"

if [ "${oom:-0}" -lt 1 ]; then
	echo "unit-caps: FAIL: the runaway job was not OOM-killed within 60s (oom_kill=${oom:-0})" >&2
	echo "--- daemon journal ---" >&2
	journalctl -u "$SERVICE" -n 60 --no-pager >&2 || true
	exit 1
fi

sleep 1
echo
echo "--- unit state after the kill ---"
echo "is-active: $(systemctl is-active "$SERVICE")"
pid_after=$(systemctl show -p MainPID --value "$SERVICE")
echo "otterd MainPID after:  $pid_after"
echo "NRestarts:             $(systemctl show -p NRestarts --value "$SERVICE")"
echo "OOMPolicy:             $(systemctl show -p OOMPolicy --value "$SERVICE")"

echo "--- runaway processes still alive (want none) ---"
pgrep -af 'runaway/main.py' || echo "(none)"

echo "--- run history ---"
runuser -u otter -- env OTTER_API_URL=http://127.0.0.1:7337 OTTER_API_TOKEN="$TOKEN" \
	"$WS/bin/otter" runs runaway --limit 3 || true

echo "--- host responsiveness ---"
host_state=$(systemctl is-system-running 2>/dev/null || true)
echo "is-system-running: $host_state"
case "$host_state" in
running | degraded) ;;
*)
	echo "unit-caps: FAIL: systemd is no longer running the host ($host_state)" >&2
	exit 1
	;;
esac
# A bounded request to the daemon is the real responsiveness check: timeout
# fails if the host cannot schedule it, and curl fails if otterd is not serving.
if ! timeout 5 curl -fsS http://127.0.0.1:7337/health; then
	echo "unit-caps: FAIL: otterd did not answer /health within 5s of the OOM kill" >&2
	exit 1
fi
echo

echo "--- daemon journal, OOM lines ---"
journalctl -u "$SERVICE" --no-pager 2>/dev/null | grep -iE 'oom|out of memory|killed process' | tail -20 || true

# The evidence clause is that otterd survives, not merely that it is restarted.
if [ "$pid_before" != "$pid_after" ]; then
	echo "unit-caps: FAIL: otterd did not survive the OOM kill (MainPID $pid_before -> $pid_after)" >&2
	exit 1
fi
if [ "$(systemctl is-active "$SERVICE")" != "active" ]; then
	echo "unit-caps: FAIL: the unit is not active after the OOM kill" >&2
	exit 1
fi

echo "unit-caps: PASS: the runaway job was OOM-killed, otterd (PID $pid_after) survived, and the host answered."
INNER
