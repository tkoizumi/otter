#!/bin/sh
# Drill: a deploy that fails midway leaves the previous release active and
# serving.
#
# Why this exists: `otter deploy` claims this property in two places --
# internal/cli/release.go:28 ("a bad candidate can never take down a working
# runtime") and internal/deploy/render.go:486 ("failure here must leave the
# previous release active") -- and nothing injected a failure to prove it.
# Claim-without-evidence is what R-19 forbids, and the observation period
# explicitly exercises upgrades (P0-06, CA-23, v0.3.0 WS6 "Scripted drills").
#
# What it does, in order:
#
#   1. builds a systemd + sshd container as the deploy TARGET and a project
#      holding one managed-Python job in a temp directory;
#   2. deploys it with the real `otter deploy` over ssh, starts a run, and
#      records the release digest and environment digest that are active;
#   3. edits the job so the next deploy carries different code, then makes
#      preparation on the host fail so the SECOND deploy fails after the
#      candidate release has been staged and before the unit is swapped. The
#      switch is a file on the host (/etc/otter/drill-preparation-fail) that
#      the drill's uv stub honours: `otter deploy` re-pushes the toolchain and
#      runs the release itself, so a trigger inside the pushed tree would be
#      reset by the deploy under test.
#   4. asserts the previous release is still active AND serving: the unit is
#      the same process, the active release link still names the old digest,
#      the health endpoint answers, and a NEW run succeeds bound to the OLD
#      release and environment digests, having really executed the old
#      snapshot.
#
# What it proves:
#   * a release that fails after staging does not become active, and the
#     runtime that was already deployed keeps running the old release;
#   * a run submitted after that failure still works, and executes the
#     previous release snapshot under its recorded identity;
#   * the assertion is falsifiable in both directions -- see DRILL_SABOTAGE.
#
# What it explicitly does NOT prove:
#   * anything about a real host. The target is a privileged container on this
#     machine, reached over published-port ssh. It is not the Castor VM and it
#     is not a VM at all; the deploy path exercised (ssh + rsync + systemd
#     unit + release + restart) is the real one, but the host is not.
#   * anything about binary rollback. Both deploys carry the same compiled
#     binaries, so the only thing that changes is the job release. A deploy
#     that swaps the unit or the binaries and then fails is not covered.
#   * real Python packaging. The host gets scripts/drill/deploy-failure-uv
#     as uv and the deploy vendors it, so no interpreter is downloaded and no
#     dependency is resolved. The injected failure is a preparation failure of
#     the shape a real one has (the environment the staged release needs cannot
#     be prepared), but real `uv sync` never runs.
#   * failure during `systemctl restart` itself. Activation is the last step of
#     a release and the unit is swapped after it; this drill fails the release,
#     which is the boundary the two code comments above make the claim about.
#
# Falsifiability (DRILL_SABOTAGE). The clean run must be green, and a run with
# one invariant deliberately broken must go red *for that reason*. Documented
# modes:
#
#   DRILL_SABOTAGE=activate-before-stage
#       Let the candidate release be staged, let the deploy fail at the release
#       step, then do what the ordering exists to prevent: point the
#       active-release link at the candidate digest and restart the unit. Step
#       4 must go red on the active-release assertion -- the runtime is now
#       serving a release whose environment was never published, so a new run
#       against it would fail with "not prepared". This is the mode the task
#       asks to be shown.
#   DRILL_SABOTAGE=stop-previous-release
#       Leave the deploy path alone and simply stop the unit before step 4.
#       Step 4 must go red on the "still serving" assertion. This is what
#       proves that half of the assertion is not vacuous.
#
# A sabotage run is expected to exit non-zero. It is red for the right reason
# only when the last lines are step 4 refusing, not some earlier setup error.
#
# Run it:
#
#   make drill DRILL=deploy-failure
#   DRILL_SABOTAGE=activate-before-stage make drill DRILL=deploy-failure
#   DRILL_SABOTAGE=stop-previous-release make drill DRILL=deploy-failure
#
# Requires docker able to run systemd as PID 1 (--privileged, --cgroupns=host,
# a writable /sys/fs/cgroup) and a Go toolchain to build the target binaries.
# A machine that cannot run the container exits non-zero and says what is
# missing: a drill that did not run has produced no evidence.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

case "${GOARCH:-$(uname -m)}" in
arm64 | aarch64) goarch=arm64 ;;
amd64 | x86_64) goarch=amd64 ;;
*)
	echo "deploy-failure: FAIL: cannot map $(uname -m) to a Go architecture; set GOARCH=amd64 or GOARCH=arm64" >&2
	exit 1
	;;
esac

if ! command -v docker >/dev/null 2>&1; then
	echo "deploy-failure: FAIL: docker is required to run the deploy target container;" >&2
	echo "deploy-failure: a deploy converges a systemd host over ssh, and there is no way to" >&2
	echo "deploy-failure: observe that on this machine without a container or a real host." >&2
	exit 1
fi
if ! docker info >/dev/null 2>&1; then
	echo "deploy-failure: FAIL: the docker daemon is not reachable" >&2
	exit 1
fi
command -v python3 >/dev/null 2>&1 || {
	echo "deploy-failure: FAIL: python3 is required on this machine to read the workspace" >&2
	echo "deploy-failure: record the target writes; it was not found on PATH." >&2
	exit 1
}

SABOTAGE=${DRILL_SABOTAGE:-}
case "$SABOTAGE" in
"" | activate-before-stage | stop-previous-release) ;;
*)
	echo "deploy-failure: unknown DRILL_SABOTAGE=$SABOTAGE" >&2
	echo "deploy-failure: valid modes: activate-before-stage, stop-previous-release" >&2
	exit 2
	;;
esac

UV_VERSION=0.5.9
JOB=counter
SLEEP_SECONDS=3
image=${OTTER_DEPLOY_IMAGE:-otter-deploy-target}
name=otter-deploy-failure-$$
# TMPDIR commonly ends in a slash on macOS, and a doubled slash would show up
# in every path the drill prints.
tmpbase=${TMPDIR:-/tmp}
while [ "$tmpbase" != "${tmpbase%/}" ]; do tmpbase=${tmpbase%/}; done
[ -n "$tmpbase" ] || tmpbase=/tmp
work=$(mktemp -d "$tmpbase/otter-deploy-failure.XXXXXX")
ws="$work/ws"
uv_cache="$work/uv-cache"
START_EPOCH=$(date +%s)
cleaning=0
wsname=""
data_dir=""
listen=""
token=""

# The drill must not write inside the checkout it runs from. Everything it
# creates lives under $work (or inside the container); this snapshot is
# compared in cleanup so a stray write is a failure, not a surprise for the
# next `git status`.
checkout_status_before=$(git -C "$root" status --porcelain 2>/dev/null || echo "(not a git checkout)")

say() { echo "deploy-failure: $*"; }
fail() {
	echo "deploy-failure: FAILED: $*" >&2
	exit 1
}

cleanup() {
	status=$?
	[ "$cleaning" -eq 1 ] && return
	cleaning=1
	docker rm -f "$name" >/dev/null 2>&1 || true
	if [ "$status" -ne 0 ]; then
		if [ -f "$work/deploy-2.out" ]; then
			echo "deploy-failure: last deploy output:" >&2
			sed 's/^/deploy-failure:   /' "$work/deploy-2.out" >&2 || true
		fi
		echo "deploy-failure: work directory kept at $work" >&2
	fi
	echo "deploy-failure: elapsed $(( $(date +%s) - START_EPOCH ))s" >&2
	if [ -n "${OTTER_DRILL_KEEP:-}" ] || { [ "$status" -ne 0 ] && [ -z "${CI:-}" ]; }; then
		echo "deploy-failure: keeping $work" >&2
	else
		rm -rf "$work"
	fi
	checkout_status_after=$(git -C "$root" status --porcelain 2>/dev/null || echo "(not a git checkout)")
	if [ "$checkout_status_after" != "$checkout_status_before" ]; then
		echo "deploy-failure: FAIL: the drill wrote inside the checkout at $root" >&2
		printf '%s\n' "$checkout_status_after" >&2
		[ "$status" -eq 0 ] && status=1
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM

# --- helpers that run against the target -------------------------------------

dexec() { docker exec "$name" "$@"; }

# otter_on_host runs the deployed CLI as the service account: that is the
# identity every job runs as, and the only one that can read the data
# directory. It runs the container's own `otter` in the workspace directory,
# the way the host's dispatcher would, with the loopback API and token the
# deploy installed.
otter_on_host() {
	docker exec \
		--user otter \
		--workdir "/opt/otter/workspaces/$wsname" \
		-e OTTER_API_URL="http://$listen" \
		-e OTTER_API_TOKEN="$token" \
		"$name" "/opt/otter/workspaces/$wsname/bin/otter" "$@"
}

# release_exec_log echoes the ground-truth execution record a run leaves inside
# a release snapshot. The job writes it into its working directory, which at run
# time is the snapshot under the data directory -- not the source tree -- so
# this also proves which snapshot actually executed.
release_exec_log() {
	digest=$1
	dexec sh -c "ls '$data_dir/.releases/'*/'$digest'/'$JOB'/executions.log 2>/dev/null | head -1"
}

# wait_for_run polls until a run settles and echoes its status.
wait_for_run() {
	id=$1
	i=0
	status=""
	while [ "$i" -lt 600 ]; do
		status=$(otter_on_host run-status "$id" 2>/dev/null | sed -n 's/^status: *//p' || true)
		case "$status" in
		succeeded | failed | timed_out | cancelled)
			printf '%s' "$status"
			return 0
			;;
		esac
		i=$((i + 1))
		sleep 0.2
	done
	fail "run $id did not settle (last status: ${status:-unknown})"
}

# --- 1. the target container and the project ---------------------------------

say "work        $work"
say "image       $image"
say "sabotage    ${SABOTAGE:-none}"
say "building the deploy-target image"
docker build -q -t "$image" -f "$here/deploy-failure.Dockerfile" "$here" >/dev/null

say "building the host CLI and the linux target binaries"
mkdir -p "$work/host-bin" "$work/linux-bin"
# `make drill` supplies OTTER_BIN, which is this checkout's own host binary.
# Run directly, build it, so the drill has one documented command either way.
if [ -n "${OTTER_BIN:-}" ]; then
	case "$OTTER_BIN" in
	/*) ;;
	*) fail "OTTER_BIN must be an absolute path, got $OTTER_BIN" ;;
	esac
	[ -x "$OTTER_BIN" ] || fail "not executable: $OTTER_BIN"
	host_otter=$OTTER_BIN
else
	( cd "$root" && scripts/go build -o "$work/host-bin/otter" ./cmd/otter )
	host_otter=$work/host-bin/otter
fi
(
	cd "$root"
	GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 scripts/go build -o "$work/linux-bin/otterd" ./cmd/otterd
	GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 scripts/go build -o "$work/linux-bin/otter" ./cmd/otter
)

say "starting the target container"
docker run -d --name "$name" --privileged --cgroupns=host \
	-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
	-p 127.0.0.1::22 \
	"$image" >/dev/null

i=0
while :; do
	state=$(dexec systemctl is-system-running 2>/dev/null || true)
	case "$state" in
	running | degraded) break ;;
	esac
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		echo "deploy-failure: FAIL: systemd never reached running inside the container." >&2
		echo "deploy-failure: this drill needs Docker able to run systemd as PID 1: --privileged," >&2
		echo "deploy-failure: --cgroupns=host, and a writable /sys/fs/cgroup (cgroup v2)." >&2
		dexec journalctl -b --no-pager 2>/dev/null | tail -40 >&2 || true
		exit 1
	fi
	sleep 1
done
ssh_port=$(docker port "$name" 22/tcp | head -1 | sed 's/.*://')
[ -n "$ssh_port" ] || fail "could not read the published ssh port"
say "target      systemd $state, ssh on 127.0.0.1:$ssh_port"

# Key authentication: the image carries no credential, so the drill mints a
# keypair on the target and copies the private half out. The container's host
# key changes every run, so the deploy's own accept-new policy records it and a
# once-per-run warning is expected noise.
dexec sh -c 'rm -f /root/.ssh/drill_key /root/.ssh/drill_key.pub
mkdir -p /root/.ssh
ssh-keygen -q -t ed25519 -N "" -f /root/.ssh/drill_key
cat /root/.ssh/drill_key.pub >> /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys'
dexec cat /root/.ssh/drill_key >"$work/key"
chmod 600 "$work/key"
dexec systemctl restart ssh
sleep 1
ssh -i "$work/key" -o BatchMode=yes -o StrictHostKeyChecking=accept-new \
	-o UserKnownHostsFile="$work/known_hosts" -o LogLevel=ERROR \
	-p "$ssh_port" root@127.0.0.1 'echo ok' >/dev/null ||
	fail "ssh to the target container did not work"

# deploy converges the target with the real command. The service account keeps
# its default (otter), so a released job runs under the same uid a real deploy
# would use.
deploy() {
	label=$1
	out="$work/deploy-$label.out"
	( cd "$ws" && OTTER_UV_CACHE="$uv_cache" "$host_otter" deploy \
		--host 127.0.0.1 --user root --port "$ssh_port" --identity "$work/key" \
		--binaries "$work/linux-bin" --timeout 5m ) >"$out" 2>&1
}

# --- the project: one managed-Python job, released by the deploy -------------

mkdir -p "$ws/jobs/$JOB"
( cd "$ws" && git init -q . ) || fail "could not make the project a git root"
python_pin=$(dexec python3 -c 'import sys; print(".".join(map(str, sys.version_info[:3])))')
say "python pin  $python_pin (the target's python3)"

# write_job renders revision N of the job. The pyproject version changes so the
# candidate's environment digest differs from the active release's, which is
# what makes the second deploy prepare something new instead of reusing the
# first environment.
write_job() {
	revision=$1
	cat >"$ws/jobs/$JOB/otter.yaml" <<YAML
version: 1
name: $JOB
description: deploy-failure drill revision $revision
entrypoint: main.py
python:
  mode: managed
timeout: 60
retry:
  attempts: 1
  backoff: none
YAML
	printf '%s\n' "$python_pin" >"$ws/jobs/$JOB/.python-version"
	cat >"$ws/jobs/$JOB/pyproject.toml" <<TOML
[project]
name = "$JOB"
version = "0.0.$revision"
requires-python = "==$python_pin"
dependencies = []
TOML
	printf 'version = 1\n' >"$ws/jobs/$JOB/uv.lock"
	cat >"$ws/jobs/$JOB/main.py" <<PY
"""Drill job: record which revision executed, then count runs in durable state."""

import os
import time

from otter import Context

ctx = Context.from_environment()
count = int(ctx.state.get("count") or 0) + 1
time.sleep($SLEEP_SECONDS)
# The workspace is writable to the unit (ReadWritePaths=), so this file is
# ground truth that does not go through the database or the API.
with open(os.path.join(os.getcwd(), "executions.log"), "a") as handle:
    handle.write("%s rev$revision %s\n" % (ctx.run_id, count))
ctx.state.set("count", count)
ctx.log.info("ran", revision="$revision", count=count)
PY
}
write_job 1

# uv on the target is a stub: `otter deploy` vendors it from the local cache,
# and the vendored copy is what the release and the daemon both resolve, so
# they agree on the environment identity without a download. See the header.
mkdir -p "$uv_cache"
cp "$here/deploy-failure-uv" "$uv_cache/uv-$UV_VERSION-linux-$goarch"
chmod 755 "$uv_cache/uv-$UV_VERSION-linux-$goarch"

# --- 2. the known-good deploy, and a run that proves it serves ---------------

say "deploy 1    known-good release"
deploy 1 || fail "the first deploy did not succeed:
$(sed 's/^/  /' "$work/deploy-1.out")"
grep -E '^(release|install|verify|done):' "$work/deploy-1.out" | sed 's/^/deploy-failure:   /'

# The workspace name and port are assigned by the host. Read them from the
# record the deploy wrote there rather than guessing.
wsname=$(dexec sh -c 'ls /opt/otter/workspaces/ | head -1')
[ -n "$wsname" ] || fail "no workspace was created on the target"
data_dir="/opt/otter/workspaces/$wsname/.otter/data"
listen=$(dexec cat "/opt/otter/workspaces/$wsname/workspace.json" |
	python3 -c 'import json,sys; print(json.load(sys.stdin)["listen"])')
token=$(dexec sh -c "grep -h '^OTTER_API_TOKEN=' /etc/otter/workspaces/$wsname.env | cut -d= -f2-")
[ -n "$listen" ] || fail "the workspace record has no listen address"
[ -n "$token" ] || fail "no API token was written to the workspace environment file"
say "workspace   $wsname at $listen"

V1_DIGEST=$(dexec sh -c "readlink -f '$data_dir/.releases/active/'*" | sed 's|.*/||')
[ -n "$V1_DIGEST" ] || fail "no active release on the target after the first deploy"
V1_ENV=$(dexec sh -c "ls '$data_dir/environments/' | grep -v '\\.lock\$' | head -1")
[ -n "$V1_ENV" ] || fail "no prepared environment on the target after the first deploy"
say "release 1   $V1_DIGEST"
say "env 1       $V1_ENV"

R1=$(otter_on_host run --no-wait "$JOB") || fail "the first run could not be submitted"
[ "$(wait_for_run "$R1")" = "succeeded" ] || fail "the first run did not succeed"
otter_on_host run-status "$R1" | grep -q "^release: *$V1_DIGEST\$" ||
	fail "the first run did not execute release $V1_DIGEST"
EXEC_LOG1=$(release_exec_log "$V1_DIGEST")
[ -n "$EXEC_LOG1" ] || fail "release 1 has no execution log on the host"
dexec cat "$EXEC_LOG1" | grep -q "^$R1 rev1 " ||
	fail "the first run left no execution record in release 1's snapshot"
say "run 1       $R1 succeeded from release 1 (revision 1 executed)"

# The daemon process the failed deploy must not disturb.
pid_before=$(dexec systemctl show -p MainPID --value "otterd-$wsname.service")
[ -n "$pid_before" ] && [ "$pid_before" != "0" ] || fail "the unit has no main process after the first deploy"
say "unit        MainPID $pid_before before the candidate deploy"

# --- 3. the candidate deploy, failed midway by injection ---------------------

write_job 2
say "deploy 2    candidate release (expected to fail after staging)"

# The injection. The candidate's environment digest differs from the active
# one because the job's pyproject changed, so preparation has real work to do;
# preparation is then made to fail on the host. Everything the candidate needs
# to be *staged* is untouched, and nothing the active release needs is touched:
# an already-prepared environment is validated from its readiness marker and
# never re-invokes uv, so the previous release keeps working throughout.
#
# The switch is a file on the host rather than anything in the drill's local
# tree because `otter deploy` re-pushes the vendored toolchain and runs
# `otter release` itself; a trigger inside the pushed tree would be reset by
# the very deploy under test.
say "inject      making preparation fail on the host"
dexec sh -c 'cat >/etc/otter/drill-preparation-fail <<EOF
Created by scripts/drill/deploy-failure.sh to inject a preparation failure.
EOF
chmod 0644 /etc/otter/drill-preparation-fail' ||
	fail "could not arm the preparation-failure injection on the host"

if deploy 2; then
	fail "the second deploy was expected to fail midway, but it succeeded"
fi
say "deploy 2    failed as injected"
grep -E 'release |activated|not activating|install:|verify:|healthy' "$work/deploy-2.out" |
	sed 's/^/deploy-failure:   /' || true

# --- the injection is at the right point, and the runtime was not reached ----

# 3a. The candidate was STAGED: its snapshot is on disk under a new digest.
CAND_DIGEST=$(sed -n 's/.*release \([0-9a-f]\{12\}\).*/\1/p' "$work/deploy-2.out" | tail -1)
if [ -n "$CAND_DIGEST" ]; then
	CAND_FULL=$(dexec sh -c "ls -d '$data_dir/.releases/'*/${CAND_DIGEST}* 2>/dev/null | head -1" || true)
else
	CAND_FULL=""
fi
if [ -z "${CAND_FULL:-}" ]; then
	# The release command prints its digest only when it gets that far; the
	# staged snapshot on disk is the authority either way.
	CAND_FULL=$(dexec sh -c "ls -dt '$data_dir/.releases/'*/*/ 2>/dev/null | head -1" || true)
	CAND_FULL=${CAND_FULL%/}
fi
[ -n "${CAND_FULL:-}" ] || fail "no candidate release snapshot was staged on the host at all"
CAND_DIGEST=${CAND_FULL##*/}
[ "$CAND_DIGEST" != "$V1_DIGEST" ] ||
	fail "the staged candidate is the active release; nothing new was staged"
dexec sh -c "test -f '$CAND_FULL/otter-release.json'" ||
	fail "the staged candidate $CAND_DIGEST has no release metadata"
CAND_ENV=$(dexec sed -n 's/^[[:space:]]*"environment": "\([^"]*\)".*/\1/p' "$CAND_FULL/otter-release.json")
[ -n "$CAND_ENV" ] || fail "could not read the staged candidate's environment digest"
[ "$CAND_ENV" != "$V1_ENV" ] ||
	fail "the candidate reuses the active environment, so preparation was not the failing step"
# An aborted preparation may leave an empty directory behind; what it must
# never leave is the readiness marker that publishes an environment to runs.
dexec sh -c "test ! -e '$data_dir/environments/$CAND_ENV/otter-ready.json'" ||
	fail "the candidate environment was published despite the injected failure"
say "staged      candidate $CAND_DIGEST is on disk but not active"
say "injected    candidate environment $CAND_ENV was never published"

# 3b. The deploy really failed at the release step rather than earlier: the
# output names the release, and activation was refused. (The MainPID check in
# step 4 is the authoritative form of "the unit was never reached".)
grep -q 'releasing' "$work/deploy-2.out" ||
	fail "the second deploy did not reach the release step, so nothing was staged"
grep -qi 'not activating' "$work/deploy-2.out" ||
	fail "the second deploy did not report a refused activation"

if [ "$SABOTAGE" = "activate-before-stage" ]; then
	# Sabotage: do what the ordering exists to prevent. Point the active
	# release at the staged candidate and restart the unit. Step 4 must now
	# fail because the release the runtime serves cannot execute.
	say "sabotage    activating the staged candidate and restarting the unit"
	pid_before=$(dexec systemctl show -p MainPID --value "otterd-$wsname.service")
	dexec sh -c "ln -sfn '$CAND_FULL' '$data_dir/.releases/active/$JOB' &&
		systemctl restart otterd-$wsname.service" ||
		fail "sabotage: could not activate the candidate"
	i=0
	until dexec curl -fsS --max-time 2 "http://$listen/health" >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt 100 ] || fail "sabotage: the daemon never answered after the restart"
		sleep 0.2
	done
fi

if [ "$SABOTAGE" = "stop-previous-release" ]; then
	# Sabotage: leave everything else as it is and take the runtime down.
	say "sabotage    stopping the unit before the serving assertion"
	pid_before=$(dexec systemctl show -p MainPID --value "otterd-$wsname.service")
	dexec systemctl stop "otterd-$wsname.service" || fail "sabotage: could not stop the unit"
fi

# --- 4. the previous release is still active and serving ---------------------

# 4a. The active release is still the previous one.
active_now=$(dexec sh -c "readlink -f '$data_dir/.releases/active/'*" | sed 's|.*/||')
if [ "$active_now" != "$V1_DIGEST" ]; then
	fail "the active release is ${active_now:-nothing}, not the previous release ($V1_DIGEST)"
fi

# 4b. It is still serving: the unit is active, and it is the same process. A
# deploy that got as far as `systemctl restart` would have changed the MainPID
# even if the release itself had failed; a deploy that took the runtime down
# while swapping the unit would leave it inactive here.
unit_state=$(dexec systemctl is-active "otterd-$wsname.service" || true)
[ "$unit_state" = "active" ] ||
	fail "the previous release is not running: the unit is $unit_state after the failed deploy"

pid_after=$(dexec systemctl show -p MainPID --value "otterd-$wsname.service" || true)
[ -n "$pid_after" ] && [ "$pid_after" != "0" ] ||
	fail "the previous release is not running: the unit is active but has no main process"
if [ "$pid_after" != "$pid_before" ]; then
	fail "the failed deploy restarted the daemon (MainPID $pid_before -> $pid_after)"
fi
say "unit        same process as before the failed deploy (MainPID $pid_after)"

dexec curl -fsS --max-time 5 "http://$listen/health" >/dev/null ||
	fail "the daemon did not answer its health endpoint after the failed deploy"

R2=$(otter_on_host run --no-wait "$JOB" 2>"$work/run-2.err") || {
	echo "deploy-failure: the previous release refused a new run:" >&2
	sed 's/^/deploy-failure:   /' "$work/run-2.err" >&2
	fail "the previous release is active but cannot run"
}
R2_STATUS=$(wait_for_run "$R2")
[ "$R2_STATUS" = "succeeded" ] || fail "the run after the failed deploy settled as $R2_STATUS"
otter_on_host run-status "$R2" |
	grep -q "^release: *$V1_DIGEST\$" ||
	fail "the run after the failed deploy did not execute release $V1_DIGEST"
otter_on_host run-status "$R2" |
	grep -q "^environment: *$V1_ENV\$" ||
	fail "the run after the failed deploy did not execute environment $V1_ENV"
EXEC_LOG2=$(release_exec_log "$V1_DIGEST")
[ -n "$EXEC_LOG2" ] || fail "release 1 lost its execution log during the failed deploy"
dexec cat "$EXEC_LOG2" | grep -q "^$R2 rev1 " ||
	fail "the run after the failed deploy did not execute revision 1's snapshot"

if [ -n "$SABOTAGE" ]; then
	# Reaching here means the sabotage broke nothing, so the clean assertion
	# would have passed vacuously. That is a drill bug, not a pass.
	fail "sabotage $SABOTAGE did not break the assertion (the drill is not falsifiable)"
fi

say "run 2       $R2 succeeded from release 1 (revision 1 executed; revision 2 was never served)"
say "unit        still active after the failed deploy; /health answered"
say "ok: a mid-deploy failure left the previous release active and serving"
