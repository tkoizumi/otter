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
# What it explicitly does NOT prove:
#   * hot rotation. There is no credential reload: `docs/secret-reload-
#     implementation-plan.md` is a plan, not implemented behavior, and this drill
#     asserts the shipped truth instead. When that feature lands, step 2 must be
#     inverted deliberately rather than quietly passing.
#   * that a child already executing observes the old value across a rotation.
#     Under restart rotation no child survives: an in-flight run is interrupted
#     by the restart, which `operations.md` documents and the crash-recovery
#     tests cover.
#
# Falsifiability (DRILL_SABOTAGE). The clean run must be green. With
# `DRILL_SABOTAGE=reload-applies` the drill writes the rotated value into the
# *daemon's* environment via `otter reload` before asserting step 2, so the
# "reload did not apply the rotation" assertion goes red -- the drill detects a
# credential reload rather than describing its absence.
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
"" | reload-applies) ;;
*)
	echo "drill: unknown DRILL_SABOTAGE=$SABOTAGE" >&2
	echo "drill: valid modes: reload-applies" >&2
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

otter stop >/dev/null 2>&1 || true
wait_stopped

say "ok: rotation is applied by restart, reload does not re-read credentials, and a missing one fails loudly"
