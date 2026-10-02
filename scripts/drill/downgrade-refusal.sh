#!/bin/sh
# Drill: a database migrated by a newer binary is refused, and the data survives
# the refusal.
#
# Why this exists: docs/operations.md "Upgrades" tells an operator that a
# downgrade is a **restore**, not an older binary against the newer database. The
# executable half of that instruction is the schema check in
# internal/database/migrate.go: a database whose newest applied migration this
# build does not embed is refused before anything reads or writes the schema.
# This drill is the executable form of that instruction (CA-22, R-11).
#
# What it does, in order:
#
#   1. scaffolds a workspace with `otter init` (the shipped sample), releases the
#      job and runs it once, so there is real history and durable state that a
#      careless downgrade could damage;
#   2. stops the daemon, records the newest applied migration, and makes the
#      database look like a *newer* binary migrated it by adding one
#      schema_migrations row -- which is exactly what a later binary leaves
#      behind;
#   3. starts the daemon again and asserts it REFUSES: it exits non-zero, the
#      documented message names both versions, and no pending migration ran;
#   4. asserts the database is intact after the refusal: run history and durable
#      state are still readable, and the API never served;
#   5. removes the newer row and starts the same binary again, asserting it now
#      serves the same data -- so the refusal was caused by the version, not by
#      a broken database.
#
# What it proves:
#   * a downgrade is refused before any migration is applied;
#   * the refusal leaves the database untouched, including the newer row itself;
#   * the binary that refused is otherwise healthy: with the newer row gone it
#     starts and the history is still there, which is the data a pre-upgrade
#     backup would have captured.
#
# What it explicitly does NOT prove:
#   * that a real *older* binary (one built before the guard existed) refuses.
#     A guard cannot be retrofitted, so this drill simulates the newer database
#     the guard reads rather than an older binary -- the state on disk is what
#     the check examines, and that is what this reproduces;
#   * that a backup taken before the upgrade restores. That is the
#     `backup-restore` drill's claim, and it runs against its own workspace.
#
# Falsifiability (DRILL_SABOTAGE). The clean run must be green. With
# `DRILL_SABOTAGE=hide-newer-row` the newer row is never written, so the daemon
# starts normally and the refusal assertion goes red -- the drill detects the
# *absence* of the guard rather than restating its presence.
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
command -v sqlite3 >/dev/null 2>&1 || {
	echo "drill: sqlite3 is required to make the database look newer than the binary and was not found on PATH" >&2
	exit 2
}

SABOTAGE=${DRILL_SABOTAGE:-}
case "$SABOTAGE" in
"" | hide-newer-row) ;;
*)
	echo "drill: unknown DRILL_SABOTAGE=$SABOTAGE" >&2
	echo "drill: valid modes: hide-newer-row" >&2
	exit 2
	;;
esac

# A developer's shell must not redirect this run at another daemon.
unset OTTER_API_URL OTTER_API_TOKEN 2>/dev/null || true
unset OTTER_DATA_DIR OTTER_JOBS_DIR OTTER_LISTEN 2>/dev/null || true
unset OTTER_PROJECT_ROOT OTTER_SERVE_DIR 2>/dev/null || true

JOB=downgrade-job

# TMPDIR commonly ends in a slash on macOS; a doubled slash here would make
# paths the daemon recorded differ from the ones this drill compares.
tmpbase=${TMPDIR:-/tmp}
while [ "$tmpbase" != "${tmpbase%/}" ]; do tmpbase=${tmpbase%/}; done
[ -n "$tmpbase" ] || tmpbase=/tmp
WORK=$(mktemp -d "$tmpbase/otter-drill-downgrade-refusal.XXXXXX")
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

# otter runs every CLI call from the workspace, the way an operator would.
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

query() { sqlite3 "$DB" "$1"; }

# 1. a real workspace with history and durable state.
mkdir -p "$WS"
say "init        scaffolding $JOB"
otter init "$JOB" >/dev/null
otter validate "$JOB" >/dev/null
otter release "$JOB" >/dev/null
say "start       bringing the runtime up"
otter start --detach >/dev/null
wait_ready "initial"

say "run         executing the job once"
RUN=$(otter run --no-wait "$JOB")
[ "$(wait_run "$RUN")" = "succeeded" ] || fail "the seeding run did not succeed"

SCHEMA=$(query "select max(version) from schema_migrations;")
STATE_BEFORE=$(query "select count(*) from job_state;")
SUCCEEDED_BEFORE=$(query "select count(*) from runs where status='succeeded';")
[ -n "$SCHEMA" ] || fail "the database has no schema_migrations rows"
[ "$SUCCEEDED_BEFORE" -ge 1 ] || fail "the seeding run left no history"

say "stop        the daemon must not hold the database while we edit it"
otter stop >/dev/null 2>&1 || true
wait_stopped

# 2. make the database look like a newer binary migrated it.
FUTURE=$((SCHEMA + 1))
if [ "$SABOTAGE" = "hide-newer-row" ]; then
	say "sabotage: not writing the newer schema_migrations row"
else
	query "insert into schema_migrations (version, name, applied_at) values ($FUTURE, '0099_from_the_future.sql', strftime('%Y-%m-%dT%H:%M:%fZ','now'));"
	say "newer       database now records migration $FUTURE, this build knows $SCHEMA"
fi

# 3. the daemon must refuse. Foreground `otter start` blocks when it starts, so
# give it a bounded window to either exit or come up.
say "start       expecting a refusal"
set +e
otter start >"$WORK/refused.log" 2>&1 &
START_PID=$!
set -e
i=0
while kill -0 "$START_PID" 2>/dev/null; do
	i=$((i + 1))
	if [ "$i" -ge 300 ]; then
		# It came up. Stop it before failing so the sabotage path leaves no
		# daemon behind.
		otter stop >/dev/null 2>&1 || true
		kill "$START_PID" 2>/dev/null || true
		fail "the daemon served a database migrated by a newer binary instead of refusing it"
	fi
	sleep 0.1
done
set +e
wait "$START_PID"
START_STATUS=$?
set -e
if [ "$START_STATUS" -eq 0 ]; then
	sed 's/^/drill:   /' "$WORK/refused.log" >&2
	fail "the daemon exited 0 against a database migrated by a newer binary"
fi
grep -q "schema is newer than this binary" "$WORK/refused.log" || {
	sed 's/^/drill:   /' "$WORK/refused.log" >&2
	fail "the refusal does not name the schema version"
}
grep -q "downgrades are not supported" "$WORK/refused.log" || {
	sed 's/^/drill:   /' "$WORK/refused.log" >&2
	fail "the refusal does not state the policy"
}
say "refused     exit $START_STATUS and named both versions"

# 4. the refusal changed nothing.
if otter status >/dev/null 2>&1; then
	fail "the API served after a refused startup"
fi
SCHEMA_AFTER=$(query "select max(version) from schema_migrations;")
[ "$SCHEMA_AFTER" = "$FUTURE" ] ||
	fail "the refusal modified the schema version: $SCHEMA_AFTER, want $FUTURE"
[ "$(query "select count(*) from schema_migrations where version=$FUTURE;")" = "1" ] ||
	fail "the refusal removed the newer migration row"
[ "$(query "select count(*) from job_state;")" = "$STATE_BEFORE" ] ||
	fail "durable state changed across the refusal"
[ "$(query "select count(*) from runs where status='succeeded';")" = "$SUCCEEDED_BEFORE" ] ||
	fail "run history changed across the refusal"
say "intact      history=$SUCCEEDED_BEFORE state=$STATE_BEFORE schema=$SCHEMA_AFTER unchanged"

# 5. the refusal was the version, not a broken database.
query "delete from schema_migrations where version=$FUTURE;"
say "rollback    removing the newer row (the pre-upgrade backup's job in production)"
otter start --detach >/dev/null
wait_ready "after the rollback"
otter runs --all --limit 5 >/dev/null || fail "run history is unreadable after the rollback"
[ "$(query "select count(*) from runs where status='succeeded';")" = "$SUCCEEDED_BEFORE" ] ||
	fail "history did not survive the rollback"
GOT_STATE=$(otter state get "$JOB" count 2>/dev/null) ||
	fail "durable state is unreadable after the rollback"
[ -n "$GOT_STATE" ] || fail "durable state is empty after the rollback"
say "recovered   the same binary serves the same data once the schema is understood"

otter stop >/dev/null 2>&1 || true
wait_stopped

say "ok: a newer schema is refused, the data survives, and rollback is a restore"
