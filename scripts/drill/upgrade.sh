#!/bin/sh
# Drill: a database written by the previous release upgrades to this build's
# schema, observably, and the runtime works afterwards.
#
# Why this exists: docs/operations.md "Upgrades" tells an operator to watch the
# journal for `migration_applied` on restart, then smoke with `otter status`,
# `otter jobs` and `otter runs`. Before the schema-visibility change nothing
# emitted that line, so the procedure asked the operator to watch for something
# that could not appear. This drill is the executable form of the procedure
# (CA-22, R-11): the workspace is built by the previous *released* binary, so the
# schema it starts from is a real released schema, not a hand-built imitation.
#
# What it does, in order:
#
#   1. resolves the previous release tag (the newest release tag reachable from
#      HEAD^), extracts that source from git and builds it into a temporary
#      directory -- the "old" binary;
#   2. with the OLD binary: `init`, `validate`, `release` -- which migrates the
#      data directory to the old schema -- and records that schema version;
#   3. with the CURRENT binary: starts the daemon against that data directory;
#   4. asserts the upgrade is visible and complete:
#        * one `migration_applied` per pending migration, in version order, with
#          the migration's own name, and none for a migration that was already
#          applied;
#        * the schema reaches this build's newest migration;
#        * `otter status`, `otter jobs` and `otter runs --all` work -- the
#          documented post-upgrade commands;
#        * a job released before the upgrade runs again after one `otter
#          release`, which is the documented "every job must be released once
#          after upgrading" step.
#
# What it proves:
#   * a real previous-release database is upgraded by this build, and the
#     operator-visible record of that upgrade is exactly one line per migration;
#   * the schema version after the upgrade is the version this build embeds;
#   * the upgraded runtime is usable, not merely started.
#
# What it explicitly does NOT prove:
#   * a downgrade. That is `downgrade-refusal`;
#   * that a managed-Python environment prepared by the old release still runs
#     after the upgrade. The scaffold job uses external Python, and a managed
#     environment cannot be prepared offline here.
#
# Falsifiability (DRILL_SABOTAGE). The clean run must be green. With
# `DRILL_SABOTAGE=skip-old-binary` the workspace is built by the CURRENT binary,
# so there is nothing pending and the drill goes red at the pending-set check --
# it detects a missing upgrade rather than merely describing one.
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
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
[ -d "$ROOT/migrations" ] || {
	echo "drill: cannot find the checkout's migrations directory from $0" >&2
	exit 2
}
for tool in git tar go sqlite3; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "drill: $tool is required to build the previous release and inspect the schema, and was not found on PATH" >&2
		exit 2
	}
done

SABOTAGE=${DRILL_SABOTAGE:-}
case "$SABOTAGE" in
"" | skip-old-binary) ;;
*)
	echo "drill: unknown DRILL_SABOTAGE=$SABOTAGE" >&2
	echo "drill: valid modes: skip-old-binary" >&2
	exit 2
	;;
esac

# The previous release is the newest release tag reachable from HEAD^: at a
# tagged release commit that is the release before it, and on a development
# branch it is the release currently deployed. DRILL_UPGRADE_FROM overrides the
# starting point for a deliberate multi-release jump.
PREVIOUS_RELEASE=${DRILL_PREVIOUS_RELEASE:-}
if [ -z "$PREVIOUS_RELEASE" ]; then
	PREVIOUS_RELEASE=$(git -C "$ROOT" describe --tags --abbrev=0 \
		--match 'v[0-9]*.[0-9]*.[0-9]*' "${DRILL_UPGRADE_FROM:-HEAD^}" 2>/dev/null || true)
fi
[ -n "$PREVIOUS_RELEASE" ] || {
	echo "drill: cannot resolve a previous release tag from HEAD^; set DRILL_PREVIOUS_RELEASE=<tag>" >&2
	echo "drill: (a full clone with tags is required: git fetch --tags)" >&2
	exit 2
}

# A developer's shell must not redirect this run at another daemon.
unset OTTER_API_URL OTTER_API_TOKEN 2>/dev/null || true
unset OTTER_DATA_DIR OTTER_JOBS_DIR OTTER_LISTEN 2>/dev/null || true
unset OTTER_PROJECT_ROOT OTTER_SERVE_DIR 2>/dev/null || true

JOB=upgrade-job

tmpbase=${TMPDIR:-/tmp}
while [ "$tmpbase" != "${tmpbase%/}" ]; do tmpbase=${tmpbase%/}; done
[ -n "$tmpbase" ] || tmpbase=/tmp
WORK=$(mktemp -d "$tmpbase/otter-drill-upgrade.XXXXXX")
WS="$WORK/ws"
DATA="$WS/.otter/data"
DB="$DATA/otter.db"
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

# otter runs every CURRENT-binary CLI call from the workspace.
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

mkdir -p "$WS"

# 1. the previous released binary, built from its tag.
OLD_BIN="$OTTER_BIN"
if [ "$SABOTAGE" = "skip-old-binary" ]; then
	say "sabotage: seeding with the current binary, so nothing will be pending"
else
	say "old         building $PREVIOUS_RELEASE from source"
	OLD_SRC="$WORK/old-src"
	OLD_OUT="$WORK/old"
	mkdir -p "$OLD_SRC" "$OLD_OUT"
	git -C "$ROOT" archive "$PREVIOUS_RELEASE" | tar -x -C "$OLD_SRC" || fail "git archive $PREVIOUS_RELEASE failed"
	(
		cd "$OLD_SRC" &&
			go build -ldflags "-X main.version=$PREVIOUS_RELEASE" -o "$OLD_OUT/otter" ./cmd/otter &&
			go build -ldflags "-X main.version=$PREVIOUS_RELEASE" -o "$OLD_OUT/otterd" ./cmd/otterd
	) || fail "building $PREVIOUS_RELEASE failed"
	OLD_BIN="$OLD_OUT/otter"
	say "old         $("$OLD_BIN" --version 2>/dev/null || echo "$PREVIOUS_RELEASE")"
fi

# 2. a workspace written by the previous release.
say "seed        init, release and migrate with the old binary"
( cd "$WS" && "$OLD_BIN" init "$JOB" >/dev/null )
( cd "$WS" && "$OLD_BIN" validate "$JOB" >/dev/null )
( cd "$WS" && "$OLD_BIN" release "$JOB" >/dev/null )
[ -f "$DB" ] || fail "the old binary did not create $DB"
OLD_SCHEMA=$(sqlite3 "$DB" "select max(version) from schema_migrations;")
[ -n "$OLD_SCHEMA" ] || fail "the old schema has no schema_migrations rows"
say "seed        database is at schema $OLD_SCHEMA"

# The pending set is derived from this build's own migrations, so the drill does
# not need updating when a release adds one.
PENDING=""
for file in "$ROOT"/migrations/*.sql; do
	[ -f "$file" ] || continue
	base=$(basename "$file")
	version=${base%%_*}
	version=$(printf '%s' "$version" | sed 's/^0*//')
	[ -n "$version" ] || version=0
	if [ "$version" -gt "$OLD_SCHEMA" ]; then
		PENDING="$PENDING $version:$base"
	fi
done
[ -n "$PENDING" ] || fail "this build has no migration newer than schema $OLD_SCHEMA; nothing to upgrade"

# 3. the current binary upgrades it.
say "start       this build against the previous release's database"
otter start --detach >/dev/null
wait_ready "upgraded"
LOG="$WS/.otter/serve/serve.log"
[ -f "$LOG" ] || fail "the daemon wrote no log at $LOG"

# 4. the upgrade is visible, exactly once per pending migration, in order.
APPLIED=$(grep -c '"event":"migration_applied"' "$LOG" || true)
WANT=$(printf '%s\n' $PENDING | wc -l | tr -d ' ')
[ "$APPLIED" = "$WANT" ] || {
	grep '"event":"migration_applied"' "$LOG" | sed 's/^/drill:   /' >&2
	fail "logged $APPLIED migration_applied records, want one per pending migration ($WANT)"
}
if grep -q '"event":"migration_failed"' "$LOG"; then
	grep '"event":"migration_failed"' "$LOG" | sed 's/^/drill:   /' >&2
	fail "the upgrade logged a migration_failed"
fi
last=0
for entry in $PENDING; do
	version=${entry%%:*}
	name=${entry#*:}
	line=$(grep "\"event\":\"migration_applied\"" "$LOG" | grep "\"version\":$version[,}]" || true)
	[ -n "$line" ] || fail "no migration_applied record for version $version"
	printf '%s' "$line" | grep -q "\"name\":\"$name\"" ||
		fail "the migration_applied record for $version does not name $name: $line"
	[ "$version" -gt "$last" ] || fail "migration_applied records are out of order at version $version"
	last=$version
done
say "applied     one record for each of:$PENDING"

NEW_SCHEMA=$(sqlite3 "$DB" "select max(version) from schema_migrations;")
EXPECTED=$(printf '%s\n' $PENDING | sed 's/:.*//' | sort -n | tail -n 1)
[ "$NEW_SCHEMA" = "$EXPECTED" ] ||
	fail "schema is at $NEW_SCHEMA after the upgrade, want $EXPECTED"
say "schema      $OLD_SCHEMA -> $NEW_SCHEMA"

# The documented post-upgrade smoke.
otter status >/dev/null || fail "otter status failed after the upgrade"
otter jobs >/dev/null || fail "otter jobs failed after the upgrade"
otter runs --all --limit 10 >/dev/null || fail "otter runs failed after the upgrade"
otter jobs | grep -q "$JOB" || fail "the upgraded runtime does not list $JOB"
say "commands    otter status, jobs and runs all work"

# The documented "release once after upgrading" step, and then a real run.
otter release "$JOB" >/dev/null || fail "otter release failed after the upgrade"
RUN=$(otter run --no-wait "$JOB")
STATUS=$(wait_run "$RUN")
[ "$STATUS" = "succeeded" ] || fail "a run after the upgrade settled as $STATUS"
say "run         $RUN succeeded after re-releasing"

otter stop >/dev/null 2>&1 || true
wait_stopped

say "ok: $PREVIOUS_RELEASE's schema upgraded to $NEW_SCHEMA, visibly, and the runtime works"
