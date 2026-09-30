#!/bin/sh
# Drill: a hot backup of a live runtime restores onto a different, empty data
# directory and the restored job still runs.
#
# Why this exists: `otter.db` on its own does not restore a runtime. Run
# history references release snapshots that live on disk beside the database
# and managed runs reference prepared environments; job identity is a marker
# inside the job's *source* directory. A database-only backup restores history
# whose code is gone -- the runs list, and nothing that can execute. This drill
# is the executable form of docs/operations.md "Backups" (P0-03, CA-21, R-08).
#
# What it does, in order:
#
#   1. scaffolds a workspace with `otter init` (the shipped sample), converts
#      the job to managed Python, releases it, and runs it to completion once;
#   2. starts a *second* run and waits until it is genuinely executing;
#   3. takes a hot backup while that run is in flight and the daemon is
#      writing: `sqlite3 .backup` for the database plus a byte copy of the
#      release snapshots, the prepared environment, the vendored uv and the
#      job source directory (marker included). The daemon is NOT stopped;
#   4. stops the daemon, moves the original data directory out of reach, and
#      restores the backup into a brand-new empty data directory served by a
#      fresh daemon process;
#   5. asserts, against the restored runtime: the daemon starts, job identity
#      survived, run history reads back, durable ctx.state survived, the
#      release and environment are present, and -- the point -- a NEW run of
#      the job executes successfully.
#
# What it proves:
#   * the database, `.releases/`, `environments/`, `tools/uv` and the job
#     source marker together are sufficient to bring a job back to *runnable*
#     on the same host from a different data directory;
#   * a backup taken while a run is in flight restores to a consistent
#     database (the in-flight run is recovered, not lost);
#   * step 5 is falsifiable: see DRILL_SABOTAGE below.
#
# What it explicitly does NOT prove:
#   * a restore onto a different HOST. The job's source directory stays at the
#     same absolute path here; the release metadata and the `.otter-id` marker
#     are path-bound. That half is the **clean-host mode** of this same entry
#     point, `DRILL_CLEAN_HOST=1`, which drives two hosts over ssh and restores
#     onto a second, initially empty machine
#     (scripts/drill/modes/backup-restore-clean-host.sh). Same host, different data
#     directory is all this mode claims.
#   * that a real `uv`-prepared environment was restored. Building a real
#     managed interpreter needs network and is out of scope for a fast,
#     offline drill, so the environment is a stub: `otter-ready.json` plus
#     `bin/python` pointing at the host interpreter. The drill therefore
#     proves that the environment *directory* is backed up and restored, not
#     that `otter prepare` can rebuild it.
#   * that `python/` and `cache/uv/` survive; they are reconstructable with
#     `otter prepare` and are copied only when present.
#
# Falsifiability (DRILL_SABOTAGE). The clean run must be green, and a run with
# one piece of the backup deliberately omitted must go red *because the
# restored job cannot run* -- not because of an unrelated error. Documented
# modes:
#
#   DRILL_SABOTAGE=drop-releases      omit `.releases/`; the restored daemon
#                                     reports "no active release" and refuses
#                                     the run.
#   DRILL_SABOTAGE=drop-environments  omit `environments/`; the restored
#                                     daemon reports the managed environment
#                                     "is not prepared" and refuses the run.
#   DRILL_SABOTAGE=drop-tools         omit `tools/uv`; the restored daemon
#                                     resolves a DIFFERENT environment digest
#                                     for the same job, cannot find it, and
#                                     refuses the run.
#   DRILL_SABOTAGE=drop-marker        copy the job source directory without
#                                     `.otter-id`; the restore is missing the
#                                     identity the database already knows, so
#                                     the restored source is a *different
#                                     instance*. This is the sabotage that makes
#                                     the identity assertion load-bearing, and it
#                                     goes red earlier than the others -- at the
#                                     restore, not at the run.
#
# Run them and compare, for example:
#
#   make drill DRILL=backup-restore
#   DRILL_SABOTAGE=drop-releases make drill DRILL=backup-restore
#   DRILL_SABOTAGE=drop-environments make drill DRILL=backup-restore
#   DRILL_SABOTAGE=drop-marker make drill DRILL=backup-restore
#
# A sabotage run is expected to exit non-zero. It is red for the right reason
# only when the last lines name the member that was omitted.
#
# Two on-disk facts this drill makes explicit, both recorded as ABSOLUTE paths
# under the data directory and both invisible until the data directory path
# changes:
#
#   * `<data>/.releases/active/<job>` is a symlink to the release directory
#     (internal/release/stage.go writes os.Symlink(dir, ...) with the absolute
#     dir);
#   * `<data>/environments/<digest>/otter-ready.json` records its `interpreter`
#     as an absolute path inside the environment directory.
#
# Copying those trees into a data directory at a different path leaves both
# pointing at the ORIGINAL data directory: the restored runtime silently serves
# the old releases and cannot find its interpreter. A restore into a different
# data directory must repoint both (step 4). docs/operations.md does not
# currently mention either step; on a clean host restored to the same absolute
# path neither is needed, which is why the gap has stayed invisible.
set -eu

# --- which drill -------------------------------------------------------------
# Two modes share this entry point:
#
#   * the default local, same-host drill -- the one whose transcript is
#     recorded. It takes no parameters and builds its own world;
#   * DRILL_CLEAN_HOST=1: the two-host **clean-host** drill
#     (scripts/drill/modes/backup-restore-clean-host.sh), which takes its hosts and
#     paths from parameters and proves the restore onto a *second, empty* host.
#     See that script's header for the parameters and for what it does not
#     prove.
#
# Both halves of a half-configured clean-host run are refused rather than
# resolved. A parameter set without the opt-in would otherwise run the local
# mode and record a transcript for a claim nobody made; the opt-in without its
# parameters would fail later with a message about ssh rather than about the
# missing value. A silent fallback either way is evidence for the wrong claim,
# which is the one thing a drill must never produce. Every variable the
# clean-host mode reads is in the list, optional ones included: setting
# DRILL_REMOTE_SUDO and nothing else is still an attempt to run that drill.
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
clean_host_seen=""
for name in DRILL_SOURCE_HOST DRILL_TARGET_HOST DRILL_SSH_KEY DRILL_JOB \
	DRILL_REMOTE_DIR DRILL_REMOTE_JOBS_DIR DRILL_REMOTE_SERVICE DRILL_REMOTE_BIN \
	DRILL_REMOTE_API_URL DRILL_REMOTE_API_TOKEN DRILL_REMOTE_ENV_FILE \
	DRILL_REMOTE_ETC_DIR DRILL_REMOTE_BACKUP_DIR DRILL_REMOTE_SUDO \
	DRILL_KEEP_TARGET DRILL_RUN_LIMIT DRILL_TRANSPORT; do
	eval "clean_host_value=\${$name:-}"
	[ -n "$clean_host_value" ] && clean_host_seen="$clean_host_seen $name"
done
case "${DRILL_CLEAN_HOST:-}" in
1)
	exec sh "$root/scripts/drill/modes/backup-restore-clean-host.sh"
	;;
"" | 0)
	if [ -n "$clean_host_seen" ]; then
		echo "drill: clean-host parameters are set:$clean_host_seen" >&2
		echo "drill: but DRILL_CLEAN_HOST is not 1, so this would run the local same-host drill" >&2
		echo "drill: refusing to run it: that transcript would be evidence for the wrong claim" >&2
		echo "drill: either pass DRILL_CLEAN_HOST=1 or unset the clean-host parameters" >&2
		exit 2
	fi
	;;
*)
	echo "drill: DRILL_CLEAN_HOST must be 1 or unset, got $DRILL_CLEAN_HOST" >&2
	exit 2
	;;
esac

# Say which drill this is, so a transcript cannot be read as the other one.
echo "drill: local same-host mode: one machine, a different data directory"

# --- the binary -------------------------------------------------------------
# `make drill` supplies OTTER_BIN. Run directly, fall back to the checkout's
# build, so a second operator has one documented command rather than a guess.
if [ -z "${OTTER_BIN:-}" ]; then
	if [ -x "$root/bin/otter" ]; then
		OTTER_BIN="$root/bin/otter"
	else
		echo "drill: OTTER_BIN is not set and $root/bin/otter does not exist" >&2
		echo "drill: run it through the build, which supplies the binary:" >&2
		echo "drill:   make drill DRILL=backup-restore" >&2
		echo "drill: or build first and pass it explicitly:" >&2
		echo "drill:   make build && OTTER_BIN=$root/bin/otter sh $0" >&2
		exit 2
	fi
fi
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
command -v sqlite3 >/dev/null 2>&1 || {
	echo "drill: sqlite3 is required for the hot backup and was not found on PATH" >&2
	exit 2
}
command -v python3 >/dev/null 2>&1 || {
	echo "drill: python3 is required for the stub managed environment and was not found on PATH" >&2
	exit 2
}
command -v readlink >/dev/null 2>&1 || {
	echo "drill: readlink is required to inspect the release activation link and was not found on PATH" >&2
	exit 2
}

# A developer's shell must not redirect this run at another daemon.
unset OTTER_API_URL OTTER_API_TOKEN 2>/dev/null || true
unset OTTER_DATA_DIR OTTER_JOBS_DIR OTTER_LISTEN 2>/dev/null || true
unset OTTER_PROJECT_ROOT OTTER_SERVE_DIR 2>/dev/null || true

SABOTAGE=${DRILL_SABOTAGE:-}
case "$SABOTAGE" in
"" | drop-releases | drop-environments | drop-tools | drop-marker) ;;
*)
	echo "drill: unknown DRILL_SABOTAGE=$SABOTAGE" >&2
	echo "drill: valid modes: drop-releases, drop-environments, drop-tools, drop-marker" >&2
	exit 2
	;;
esac

JOB=drill-job
PIN=3.13.5
UV_STUB_VERSION="uv 0.0.0-drill"
SLEEP_SECONDS=6

# TMPDIR commonly ends in a slash on macOS; a doubled slash here would make
# every path this drill compares by prefix differ from the path the daemon
# recorded (filepath.Abs cleans it), so strip it before mktemp.
tmpbase=${TMPDIR:-/tmp}
while [ "$tmpbase" != "${tmpbase%/}" ]; do tmpbase=${tmpbase%/}; done
[ -n "$tmpbase" ] || tmpbase=/tmp
WORK=$(mktemp -d "$tmpbase/otter-drill-backup-restore.XXXXXX")
WS="$WORK/ws"
DATA_ORIG="$WS/.otter/data"
DATA_RESTORE="$WORK/restore-data"
BACKUP="$WORK/backup"
EXEC_LOG="$WORK/exec/executions.log"
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

	# Stop only the runtime this workspace recorded. A failure to stop must not
	# mask the reason the drill failed.
	( cd "$WS" 2>/dev/null && "$OTTER_BIN" stop >/dev/null 2>&1 ) || true

	if [ "$status" -ne 0 ]; then
		echo "drill: failed; last daemon log lines:" >&2
		tail -n 40 "$WS/.otter/serve/serve.log" >&2 2>/dev/null || true
		echo "drill: workspace was $WORK" >&2
	fi
	echo "drill: elapsed $(( $(date +%s) - START_EPOCH ))s" >&2

	# Keep the workspace for a human debugging locally; CI gets the log above
	# and nothing left behind.
	if [ -n "${OTTER_DRILL_KEEP:-}" ] || { [ "$status" -ne 0 ] && [ -z "${CI:-}" ]; }; then
		echo "drill: keeping $WORK" >&2
	else
		rm -rf "$WORK"
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM

# otter runs every CLI call from the workspace, the way an operator would.
otter() {
	( cd "$WS" && "$OTTER_BIN" "$@" )
}

# wait_ready blocks until the workspace's API answers.
wait_ready() {
	label=$1
	i=0
	until otter status >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt 600 ] || fail "$label runtime did not become ready"
		sleep 0.1
	done
}

# wait_state blocks until the job's durable state equals want.
wait_state() {
	want=$1
	label=$2
	i=0
	got=""
	until got=$(otter state get "$JOB" count 2>/dev/null) && [ "$got" = "$want" ]; do
		i=$((i + 1))
		[ "$i" -lt 600 ] || fail "$label: state count=${got:-unset}, want $want"
		sleep 0.1
	done
}

# wait_run blocks until the run reaches a terminal status and echoes it.
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

# copy_member copies one backup member, unless the operator sabotaged it.
copy_member() {
	member=$1
	src=$2
	[ -e "$src" ] || return 0
	if [ "$SABOTAGE" = "drop-$member" ]; then
		say "sabotage: omitting $member from the backup"
		return 0
	fi
	cp -a "$src" "$BACKUP/$member"
}

# --- 1. a workspace with a job, a release, and a succeeded run ---------------

say "workspace   $WS"
mkdir -p "$WS" "$WORK/exec" "$BACKUP" "$DATA_RESTORE"
cd "$WS"
"$OTTER_BIN" init "$JOB" >/dev/null

# The scaffolded job is external Python and counts its own runs. It is turned
# into a managed-Python job so the drill covers `environments/` too, and it
# records every execution in a file outside both data directories, so
# "the job ran" is checkable independently of run status.
cat >"$JOB/otter.yaml" <<YAML
version: 1
name: $JOB
description: Drill job that counts runs in durable state.
entrypoint: main.py
python:
  mode: managed
env:
  LOG_LEVEL: info
timeout: 60
retry:
  attempts: 1
  backoff: none
YAML
printf '%s\n' "$PIN" >"$JOB/.python-version"
cat >"$JOB/pyproject.toml" <<TOML
[project]
name = "$JOB"
version = "0.0.0"
requires-python = "==$PIN"
dependencies = []
TOML
printf '# Drill stub lockfile: no dependencies to resolve.\nversion = 1\n' >"$JOB/uv.lock"
cat >"$JOB/main.py" <<PY
"""Drill entrypoint: sleep, record the execution outside the database, count."""

import time

from otter import Context

from logic import next_count

ctx = Context.from_environment()
count = next_count(ctx.state.get("count"))
time.sleep($SLEEP_SECONDS)
with open("$EXEC_LOG", "a") as handle:
    handle.write("%s %s\n" % (ctx.run_id, count))
ctx.state.set("count", count)
ctx.log.info("ran", count=count)
PY

"$OTTER_BIN" validate "$JOB" >/dev/null
say "released job is managed Python, pinned to $PIN"

# A vendored uv under the data directory is part of the environment identity
# and part of the backup. This stub fixes the version string the identity is
# derived from and refuses every real subcommand, so the drill never reaches
# the network and the digest is the same on every host.
mkdir -p "$DATA_ORIG/tools/uv"
cat >"$DATA_ORIG/tools/uv/uv" <<UV
#!/bin/sh
case "\$1" in
--version)
	echo "$UV_STUB_VERSION"
	exit 0
	;;
*)
	echo "uv: drill stub refuses '\$1'" >&2
	exit 1
	;;
esac
UV
chmod +x "$DATA_ORIG/tools/uv/uv"

# The first release stages the snapshot and then fails at environment
# preparation (the stub cannot build one). That is how the drill learns the
# environment digest the run path will ask for. The staged snapshot is the
# real artifact; only the environment is stubbed.
#
# --skip-egress-check keeps the failure where it is expected: the stub uv above
# is what refuses, and the drill is offline by design (see the header). Without
# the flag the release would first probe PyPI and python-build-standalone, which
# turns a stub-refusal into a network test.
say "staging release (environment preparation fails here by design)"
if "$OTTER_BIN" release --skip-egress-check "$JOB" >"$WORK/release-1.out" 2>&1; then
	fail "the first release was expected to fail at environment preparation, but it succeeded"
fi
tail -n 1 "$WORK/release-1.out" | sed 's/^/drill:   /'

META=""
for candidate in "$DATA_ORIG"/.releases/*/*/otter-release.json; do
	[ -f "$candidate" ] || continue
	META=$candidate
	break
done
[ -n "$META" ] || fail "no staged release metadata found under $DATA_ORIG/.releases"

JOBID=$(sed -n 's/^[[:space:]]*"job": "\([^"]*\)".*/\1/p' "$META")
REL_DIGEST=$(sed -n 's/^[[:space:]]*"digest": "\([^"]*\)".*/\1/p' "$META")
ENV_DIGEST=$(sed -n 's/^[[:space:]]*"environment": "\([^"]*\)".*/\1/p' "$META")
[ -n "$JOBID" ] || fail "could not read the job identity from $META"
[ -n "$REL_DIGEST" ] || fail "could not read the release digest from $META"
[ -n "$ENV_DIGEST" ] || fail "could not read the environment digest from $META"
say "job         $JOBID"
say "release     $REL_DIGEST"
say "environment $ENV_DIGEST"

# The stub environment: a readiness marker plus an interpreter inside the
# environment directory, exactly the shape pyenv.GetReady validates. It is a
# symlink to the host interpreter because building a real managed Python is
# neither offline nor fast (see the header).
PYTHON=$(command -v python3 2>/dev/null || true)
[ -n "$PYTHON" ] || fail "python3 is required for the stub managed environment and was not found on PATH"
ENVDIR="$DATA_ORIG/environments/$ENV_DIGEST"
mkdir -p "$ENVDIR/bin" || fail "could not create $ENVDIR"
ln -s "$PYTHON" "$ENVDIR/bin/python"
cat >"$ENVDIR/otter-ready.json" <<JSON
{
  "job": "$JOBID",
  "python": "$PIN",
  "digest": "$ENV_DIGEST",
  "inputs_digest": "",
  "policy": "",
  "interpreter": "$ENVDIR/bin/python",
  "uv_version": "$UV_STUB_VERSION"
}
JSON
say "stub environment written under $ENVDIR"

say "releasing with the stub environment"
"$OTTER_BIN" release --skip-egress-check "$JOB" >/dev/null || fail "release failed with a prepared environment"

"$OTTER_BIN" start --detach >/dev/null
wait_ready "initial"
say "initial runtime ready; data $DATA_ORIG"

R1=$(otter run --no-wait "$JOB") || fail "the first run could not be submitted"
wait_state 1 "first run"
[ "$(wait_run "$R1")" = "succeeded" ] || fail "the first run did not succeed"
say "run 1       $R1 succeeded (state count=1)"

# --- 2. a run in flight, and 3. the hot backup -------------------------------

R2=$(otter run --no-wait "$JOB") || fail "the second run could not be submitted"
i=0
running=0
while [ "$i" -lt 600 ]; do
	status=$(otter run-status "$R2" 2>/dev/null | sed -n 's/^status: *//p' || true)
	if [ "$status" = "running" ]; then
		running=1
		break
	fi
	case "$status" in
	succeeded | failed | timed_out | cancelled)
		fail "run 2 settled as $status before it could be observed running; the backup would not be hot"
		;;
	esac
	i=$((i + 1))
	sleep 0.1
done
[ "$running" -eq 1 ] || fail "run 2 never reached running"

say "run 2       $R2 is running; taking the hot backup (daemon still writing)"
sqlite3 "$DATA_ORIG/otter.db" ".backup '$BACKUP/otter.db'" ||
	fail "sqlite3 .backup failed"
copy_member releases "$DATA_ORIG/.releases"
copy_member environments "$DATA_ORIG/environments"
copy_member tools "$DATA_ORIG/tools"
copy_member python "$DATA_ORIG/python"
copy_member cache "$DATA_ORIG/cache"
cp -a "$WS/$JOB" "$BACKUP/source" || fail "could not copy the job source directory"
if [ "$SABOTAGE" = "drop-marker" ]; then
	# Identity is the marker inside the source directory. Backing the directory
	# up without it is exactly the mistake the identity assertion exists to
	# catch: the database still knows the old id, and the restored path would be
	# registered as a NEW instance with empty state.
	say "sabotage: omitting .otter-id from the backed-up source directory"
	rm -f "$BACKUP/source/.otter-id"
else
	[ -f "$BACKUP/source/.otter-id" ] || fail "the backed-up source directory has no .otter-id marker"
fi

# Ground truth for the assertions below comes from the backup itself, not from
# the live runtime, because the live runtime keeps moving after the snapshot.
EXPECT_STATE=$(sqlite3 "$BACKUP/otter.db" \
	"select value from job_state where job_id='$JOBID' and key='count';") ||
	fail "could not read state from the backup database"
[ -n "$EXPECT_STATE" ] || fail "the backup database holds no state for job $JOBID"
case "$EXPECT_STATE" in
'' | *[!0-9]*) fail "the backed-up state value is not a number: $EXPECT_STATE" ;;
esac
EXPECT_AFTER=$((EXPECT_STATE + 1))
say "backup      ok (database + ${SABOTAGE:-nothing omitted}); state count=$EXPECT_STATE in the snapshot"

# Let run 2 finish before stopping the daemon, so the shutdown is clean.
R2_FINAL=$(wait_run "$R2")
say "run 2       settled as $R2_FINAL"

# --- 4. restore into a different, empty data directory -----------------------

otter stop >/dev/null || fail "could not stop the initial runtime"
[ -d "$DATA_RESTORE" ] || fail "$DATA_RESTORE does not exist"
[ -z "$(ls -A "$DATA_RESTORE" 2>/dev/null)" ] || fail "$DATA_RESTORE was not empty before the restore"

cp "$BACKUP/otter.db" "$DATA_RESTORE/otter.db" || fail "could not restore the database"
[ ! -e "$DATA_RESTORE/otter.db-wal" ] && [ ! -e "$DATA_RESTORE/otter.db-shm" ] ||
	fail "a stale WAL or SHM file was restored beside the database"

restore_member() {
	member=$1
	dest=$2
	[ -e "$BACKUP/$member" ] || return 0
	cp -a "$BACKUP/$member" "$dest"
}
if [ "$SABOTAGE" != "drop-releases" ]; then
	restore_member releases "$DATA_RESTORE/.releases"
fi
if [ "$SABOTAGE" != "drop-environments" ]; then
	restore_member environments "$DATA_RESTORE/environments"
fi
if [ "$SABOTAGE" != "drop-tools" ]; then
	restore_member tools "$DATA_RESTORE/tools"
fi
restore_member python "$DATA_RESTORE/python"
restore_member cache "$DATA_RESTORE/cache"

# The source directory is restored from the backup at the same absolute path:
# identity, and the `source` recorded in each release, are path-bound.
rm -rf "$WS/$JOB" || fail "could not clear the job source directory before restoring it"
cp -a "$BACKUP/source" "$WS/$JOB" || fail "could not restore the job source directory"
[ -f "$WS/$JOB/.otter-id" ] || fail "the restored source directory lost its .otter-id marker"

# Two things on disk record ABSOLUTE paths under the data directory: the
# release activation symlink (internal/release/stage.go) and each environment's
# readiness marker, whose `interpreter` is `<data>/environments/<digest>/bin/python`.
# Restoring into a data directory at a different path must repoint both, or the
# restored runtime silently resolves releases through the original data
# directory and fails to find its interpreter. docs/operations.md does not
# currently mention either step.
phys_orig=$(cd "$DATA_ORIG" 2>/dev/null && pwd -P) || phys_orig=""
# repoint_prefix echoes the original-data-directory prefix an absolute path
# carries, accepting both this shell's spelling and the physical one.
repoint_prefix() {
	case "$1" in
	"$DATA_ORIG"/*)
		printf '%s' "$DATA_ORIG"
		return 0
		;;
	esac
	if [ -n "$phys_orig" ]; then
		case "$1" in
		"$phys_orig"/*)
			printf '%s' "$phys_orig"
			return 0
			;;
		esac
	fi
	return 1
}
# repoint echoes the restored-data-directory spelling of an absolute path.
repoint() {
	old=$1
	prefix=$(repoint_prefix "$old") || return 1
	printf '%s' "$DATA_RESTORE/${old#"$prefix"/}"
}

LINK="$DATA_RESTORE/.releases/active/$JOBID"
if [ -L "$LINK" ]; then
	target=$(readlink "$LINK")
	if ! new_target=$(repoint "$target"); then
		fail "the active release link points outside the original data directory: $target"
	fi
	rm -f "$LINK"
	ln -s "$new_target" "$LINK"
	say "restore     repointed active release link into the restored data directory"
	resolved=$(readlink "$LINK")
	case "$resolved" in
	"$DATA_RESTORE"/*) ;;
	*) fail "active release link still resolves outside the restored data directory: $resolved" ;;
	esac
	[ -e "$LINK" ] || fail "active release link $LINK does not resolve: the release snapshot is missing"
fi

for marker in "$DATA_RESTORE"/environments/*/otter-ready.json; do
	[ -f "$marker" ] || continue
	old_interpreter=$(sed -n 's/.*"interpreter": "\([^"]*\)".*/\1/p' "$marker")
	[ -n "$old_interpreter" ] || fail "an environment readiness marker has no interpreter: $marker"
	if new_interpreter=$(repoint "$old_interpreter"); then
		sed "s|\"interpreter\": \"[^\"]*\"|\"interpreter\": \"$new_interpreter\"|" \
			"$marker" >"$marker.new" || fail "could not rewrite $marker"
		mv "$marker.new" "$marker" || fail "could not replace $marker"
		say "restore     repointed an environment interpreter into the restored data directory"
	fi
	[ -e "$new_interpreter" ] || fail "the restored environment interpreter does not exist: $old_interpreter"
done

# Put the original data directory out of reach. The restored runtime must
# serve the restored copy, not the one it was backed up from.
mv "$DATA_ORIG" "$WORK/data-origin" || fail "could not move the original data directory aside"
[ ! -e "$DATA_ORIG" ] || fail "the original data directory is still in place"
say "restore     data restored to $DATA_RESTORE; original moved to $WORK/data-origin"

"$OTTER_BIN" start --detach --data "$DATA_RESTORE" >/dev/null ||
	fail "the fresh daemon did not start against the restored data directory"
wait_ready "restored"
say "restored    fresh daemon is serving the restored data directory"

# --- 5. the restored runtime is a runtime ------------------------------------

# 5a. manifests resolve and job identity survived.
IDENTITY=$(otter inspect "$JOB" 2>/dev/null | sed -n 's/^job: *//p' | head -n 1) ||
	fail "otter inspect failed against the restored runtime"
[ "$IDENTITY" = "$JOBID" ] ||
	fail "job identity changed across the restore: $JOBID -> ${IDENTITY:-missing}"
otter jobs >"$WORK/jobs.out" 2>&1 || fail "otter jobs failed against the restored runtime"
grep -q "$JOBID" "$WORK/jobs.out" || fail "otter jobs does not list the restored job"
say "identity    survived ($JOBID)"

# 5b. run history reads back through the CLI.
if ! otter runs --all --limit 100 >"$WORK/runs.out" 2>&1; then
	fail "otter runs failed against the restored runtime"
fi
grep -q "$R1" "$WORK/runs.out" || fail "the restored history does not contain run 1 ($R1)"
grep -q "$R2" "$WORK/runs.out" || fail "the restored history does not contain run 2 ($R2)"
say "history     run 1 and the in-flight run 2 are both present"

# 5c. durable ctx.state survived the restore.
GOT_STATE=$(otter state get "$JOB" count) ||
	fail "otter state get failed against the restored runtime"
[ "$GOT_STATE" = "$EXPECT_STATE" ] ||
	fail "durable state did not survive: backup has $EXPECT_STATE, restored runtime reports $GOT_STATE"
say "state       count=$GOT_STATE survived"

# 5d. THE POINT: the restored job executes successfully again. This runs
# before the on-disk snapshot checks on purpose: a sabotage run must go red
# here, refused by the restored runtime, and not on an inventory assertion
# somewhere else.
say "executing   a new run on the restored runtime"
if ! NEW=$(otter run --no-wait "$JOB" 2>"$WORK/run-refused.err"); then
	echo "drill: the restored runtime refused to submit a run:" >&2
	sed 's/^/drill:   /' "$WORK/run-refused.err" >&2
	fail "the restored job cannot run"
fi
NEW_STATUS=$(wait_run "$NEW")
[ "$NEW_STATUS" = "succeeded" ] || fail "the restored run $NEW settled as $NEW_STATUS"

# The new run must have bound to the RESTORED release and environment, and it
# must have really executed the snapshot (the job appends its run id outside
# the database).
NEW_STATUS_OUT=$(otter run-status "$NEW")
printf '%s\n' "$NEW_STATUS_OUT" | grep -q "^release: *$REL_DIGEST$" ||
	fail "the restored run did not bind to the restored release $REL_DIGEST"
printf '%s\n' "$NEW_STATUS_OUT" | grep -q "^environment: *$ENV_DIGEST$" ||
	fail "the restored run did not bind to the restored environment $ENV_DIGEST"
grep -q "^$NEW " "$EXEC_LOG" || fail "the restored run $NEW left no execution record"
RSD=$(sqlite3 "$DATA_RESTORE/otter.db" "select release_source_dir from runs where id='$NEW';")
case "$RSD" in
"$DATA_RESTORE"/*) ;;
*) fail "the restored run executed from outside the restored data directory: ${RSD:-empty}" ;;
esac
wait_state "$EXPECT_AFTER" "post-restore run"
say "executed    run $NEW succeeded from $RSD"

# 5e. the release and environment snapshots are on disk in the restored data
# directory. The run above already proved the runtime can use them; these are
# the inventory checks that the right members were copied.
[ -f "$DATA_RESTORE/.releases/$JOBID/$REL_DIGEST/otter-release.json" ] ||
	fail "the restored release snapshot is missing"
[ -f "$DATA_RESTORE/environments/$ENV_DIGEST/otter-ready.json" ] ||
	fail "the restored environment is missing"
say "snapshots   release and environment present in the restored data directory"

say "ok: hot backup restored onto a fresh data directory; the job runs again"
