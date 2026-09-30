#!/bin/sh
# P0-10 — liveness detection is a dead-man's switch, not a positive check.
#
# Claim (CA-31, R-05): when the runtime stops answering, the heartbeat stops
# pinging, so an external switch fires without anyone watching a terminal.
#
# The runtime under test is a real `otterd` from this checkout on a loopback
# port. The dead-man service is stood in for by a plain HTTP server that records
# every request it receives, so "did we ping?" is read off a log rather than
# asserted from the script's own opinion.
#
# Falsifiability (DRILL_SABOTAGE=ping-always): replace the check with the naive
# positive version that pings whether or not the runtime is healthy. The drill
# must go red on the FIRST assertion, because a positive check pings into the
# void and keeps the switch quiet while the runtime is dead. If that run passes,
# this drill is only testing that a shell script can exit non-zero.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
OTTERD_BIN=${OTTERD_BIN:-$root/bin/otterd}

[ -x "$OTTERD_BIN" ] || { echo "liveness: not executable: $OTTERD_BIN (run make build)" >&2; exit 2; }
unset OTTER_API_URL OTTER_API_TOKEN 2>/dev/null || true

SABOTAGE=${DRILL_SABOTAGE:-}
case "$SABOTAGE" in
"" | ping-always) ;;
*) echo "liveness: unknown DRILL_SABOTAGE=$SABOTAGE (known: ping-always)" >&2; exit 2 ;;
esac

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-liveness.XXXXXX")
cleaning=0
daemon_pid=""
receiver_pid=""

fail() {
	echo "liveness: FAILED: $1" >&2
	exit 1
}

free_port() {
	python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
}

# pings reads the receiver's log. The count is what the switch would see.
pings() {
	count=$(grep -c 'GET /heartbeat' "$work/receiver.log" 2>/dev/null || true)
	echo "${count:-0}"
}

cleanup() {
	status=$?
	[ "$cleaning" -eq 1 ] && return
	cleaning=1
	[ -n "$daemon_pid" ] && kill "$daemon_pid" 2>/dev/null || true
	[ -n "$receiver_pid" ] && kill "$receiver_pid" 2>/dev/null || true
	if [ "$status" -ne 0 ]; then
		echo "liveness: workspace kept at $work" >&2
		[ -f "$work/daemon.log" ] && tail -n 20 "$work/daemon.log" >&2 || true
	else
		rm -rf "$work"
	fi
}
trap cleanup EXIT INT TERM

# --- the stand-in dead-man service -------------------------------------------
# `/heartbeat` must answer 2xx: the real service does, and the heartbeat treats a
# failed delivery as a failure. python's http.server serves files, so the path
# exists as an empty file.
mkdir -p "$work/served"
: >"$work/served/heartbeat"
receiver_port=$(free_port)
python3 -m http.server "$receiver_port" --bind 127.0.0.1 --directory "$work/served" >"$work/receiver.log" 2>&1 &
receiver_pid=$!
HB_URL="http://127.0.0.1:$receiver_port/heartbeat"
ready=0
for _ in $(seq 1 50); do
	if curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$receiver_port/__ready"; then ready=1; break; fi
	sleep 0.2
done
[ "$ready" -eq 1 ] || fail "the stand-in dead-man service did not start"
echo "liveness: receiver $HB_URL (a ping is a GET /heartbeat in its log)"

# check runs the heartbeat, or the naive positive check under sabotage. It reads
# $API and $HB_URL from the environment of this script.
check() {
	if [ "$SABOTAGE" = ping-always ]; then
		curl -fsS --max-time 5 "$HB_URL" >/dev/null 2>&1
		return $?
	fi
	OTTER_API_URL=$API OTTER_HEARTBEAT_URL=$HB_URL sh "$root/scripts/heartbeat.sh"
}

# --- 1. no runtime: silence, and a non-zero exit -----------------------------
dead_port=$(free_port)
API="http://127.0.0.1:$dead_port"
before=$(pings)
if check; then
	fail "the check succeeded although nothing was listening on $API"
fi
after=$(pings)
[ "$after" -eq "$before" ] || fail "the check pinged although the runtime was down: the switch would never fire"
echo "liveness: down      no runtime, no ping, non-zero exit"

# --- 2. a real runtime: it pings ---------------------------------------------
api_port=$(free_port)
API="http://127.0.0.1:$api_port"
# The daemon needs a jobs root it can scan; an empty one answers /health, and
# keeping it under $work is part of why this drill writes nothing into the
# checkout.
mkdir -p "$work/jobs"
"$OTTERD_BIN" --data "$work/data" --jobs "$work/jobs" --listen "127.0.0.1:$api_port" >"$work/daemon.log" 2>&1 &
daemon_pid=$!
up=0
for _ in $(seq 1 60); do
	if curl -fsS --max-time 1 "$API/health" >/dev/null 2>&1; then up=1; break; fi
	kill -0 "$daemon_pid" 2>/dev/null || break
	sleep 0.5
done
[ "$up" -eq 1 ] || fail "otterd did not answer $API/health"
before=$(pings)
check || fail "the check failed against a healthy runtime answering $API/health"
after=$(pings)
[ "$after" -gt "$before" ] || fail "the check did not ping a healthy runtime"
echo "liveness: up        real otterd on $api_port, ping delivered"

# --- 3. the runtime dies: the pings stop -------------------------------------
kill "$daemon_pid" 2>/dev/null || true
wait "$daemon_pid" 2>/dev/null || true
daemon_pid=""
before=$(pings)
if check; then
	fail "the check succeeded after the runtime was killed"
fi
after=$(pings)
[ "$after" -eq "$before" ] || fail "the check pinged after the runtime was killed"
echo "liveness: killed    pings stopped, so the switch fires"

echo "liveness: ok: pings track the runtime and stop when it dies"
