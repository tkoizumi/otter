#!/bin/sh
# Verifies scripts/soak/check-destinations.py against a stand-in ClickHouse.
#
# The point of the checker is that a run exiting 0 says nothing about whether the
# rows landed, so the cases that matter are the ones where a job looks fine and
# the destination does not: a table that was never created (P0-13 seam 2), rows
# filed under the wrong dataset (seam 1), and data that has gone stale.
#
# "We could not look" must also be distinguishable from "the destination is
# empty", so the transport failures are asserted to exit 2, not 1.
set -u

here=$(cd "$(dirname "$0")" && pwd)
check="$here/check-destinations.py"
fake="$here/fake-clickhouse.py"
work=$(mktemp -d)
passed=0
failed=0

cleanup() {
    [ -n "${server_pid:-}" ] && kill "$server_pid" 2>/dev/null
    rm -rf "$work"
}
trap cleanup EXIT INT TERM

ok()   { passed=$((passed + 1)); printf '  ok   %s\n' "$1"; }
bad()  { failed=$((failed + 1)); printf '  FAIL %s\n' "$1"; }

assert_contains() {
    if grep -qF "$2" "$3"; then ok "$1"; else bad "$1 (wanted: $2)"; fi
}
assert_status() {
    if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (exit $2, wanted $3)"; fi
}

start_server() {
    FAKE_CH_SCENARIO="$1" FAKE_CH_NEWEST="${2:-2026-10-15 06:50:00}" \
        FAKE_CH_REQUIRE_ACCESS="${3:-0}" \
        python3 "$fake" >"$work/port" 2>"$work/port.err" &
    server_pid=$!
    tries=0
    while [ ! -s "$work/port" ] && [ "$tries" -lt 100 ]; do
        sleep 0.05
        tries=$((tries + 1))
    done
    port=$(sed -n 's/^PORT=//p' "$work/port")
    if [ -z "$port" ]; then
        echo "fake ClickHouse did not start: $(cat "$work/port.err")" >&2
        exit 1
    fi
    CLICKHOUSE_URL="http://127.0.0.1:$port"
    export CLICKHOUSE_URL
}

stop_server() {
    [ -n "${server_pid:-}" ] && kill "$server_pid" 2>/dev/null
    wait "$server_pid" 2>/dev/null
    server_pid=
    : >"$work/port"
}

# The suite measures freshness against a fixed instant.
now=2026-10-15T07:00:00Z
export CLICKHOUSE_DATABASE=castor
export CLICKHOUSE_USER=otter_check
export CLICKHOUSE_PASSWORD=hunter2
export CLICKHOUSE_ACCESS_CLIENT_ID=access-id
export CLICKHOUSE_ACCESS_CLIENT_SECRET=access-secret

echo "check-destinations"

# 1. Every destination holds recent rows for its own dataset.
start_server healthy
python3 "$check" --now "$now" >"$work/out" 2>&1
status=$?
assert_status "a healthy destination set exits 0" "$status" 0
assert_contains "it reports the row count it read" "42 row(s)" "$work/out"
assert_contains "and every destination passes" "4 checked, 4 passed, 0 failed" "$work/out"
stop_server

# 2. A destination table that was never created. P0-13 seam 2: jobs deliberately
#    do not create it, so this is the failure an operator must see first.
start_server missing-table
python3 "$check" --now "$now" >"$work/out" 2>&1
status=$?
assert_status "a missing destination table exits 1" "$status" 1
assert_contains "and names the table that does not exist" "does not exist" "$work/out"
assert_contains "and still reports the other destinations" "4 checked, 0 passed, 4 failed" "$work/out"
stop_server

# 3. Rows exist, but none for this job's dataset. P0-13 seam 1: a valid dataset
#    id naming the wrong dataset files every row under it silently.
start_server empty
python3 "$check" --now "$now" >"$work/out" 2>&1
status=$?
assert_status "rows under the wrong dataset exit 1" "$status" 1
assert_contains "and say which dataset had none" "no rows for dataset" "$work/out"
stop_server

# 4. The table is fine and full, and nothing has written to it in two days --
#    past the bound, which is deliberately a day. Three schedules would flag a
#    quiet upstream, because these importers write nothing when nothing changed.
start_server stale "2026-10-13 03:00:00"
python3 "$check" --now "$now" >"$work/out" 2>&1
status=$?
assert_status "stale data exits 1" "$status" 1
assert_contains "and says how old it is" "past the 86400s bound" "$work/out"
stop_server

# 5. The credential the checker must send. The stand-in refuses without the
#    Access service tokens, so this passes only if they are really on the wire.
start_server healthy "2026-10-15 06:50:00" 1
python3 "$check" --now "$now" >"$work/out" 2>&1
status=$?
assert_status "the Access service tokens are sent" "$status" 0
stop_server

# 6. "We could not look" is not "the destination is empty". A 403 from the edge
#    is a credential problem and exits 2, so it can never read as an empty table.
start_server forbidden
python3 "$check" --now "$now" >"$work/out" 2>&1
status=$?
assert_status "an edge refusal exits 2, not 1" "$status" 2
assert_contains "and names Cloudflare Access rather than the database" "Cloudflare Access" "$work/out"
stop_server

# 7. Nothing to measure against: an unset endpoint is a configuration failure.
saved="$CLICKHOUSE_URL"
unset CLICKHOUSE_URL
python3 "$check" --now "$now" >"$work/out" 2>&1
status=$?
assert_status "an unset CLICKHOUSE_URL exits 2" "$status" 2
assert_contains "and says so" "CLICKHOUSE_URL is not set" "$work/out"
CLICKHOUSE_URL="$saved"
export CLICKHOUSE_URL

# 8. The machine-readable verdict carries the same facts.
start_server healthy
python3 "$check" --now "$now" --json >"$work/out" 2>&1
status=$?
assert_status "the JSON verdict exits 0 when healthy" "$status" 0
if python3 - "$work/out" <<'PY'
import json, sys
verdict = json.load(open(sys.argv[1]))
assert verdict["checked"] == 4, verdict
assert verdict["failed"] == 0, verdict
assert verdict["results"][0]["newest"] == "2026-10-15T06:50:00Z", verdict["results"][0]
assert verdict["results"][0]["age_seconds"] == 600, verdict["results"][0]
PY
then ok "and parses with the newest row's age"; else bad "the JSON verdict is wrong"; fi
stop_server

echo "check-destinations: $passed passed, $failed failed"
[ "$failed" -eq 0 ]
