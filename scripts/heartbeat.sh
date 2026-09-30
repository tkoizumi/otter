#!/bin/sh
# Dead-man's-switch heartbeat for otterd.
#
# A positive check — "curl /health and alert if it fails" — is not enough. If the
# checker itself dies, the host loses egress, or the cron entry is removed,
# nobody finds out, because the thing that would have reported the problem is the
# thing that is gone. A dead-man's switch inverts it: this script pings an
# external service only while the runtime answers, so *silence* is the alarm, and
# it still arrives when this host can no longer send anything at all.
#
#   OTTER_HEARTBEAT_URL   the dead-man service to ping (required)
#   OTTER_API_URL         the daemon's API (default http://127.0.0.1:7337)
#   OTTER_HEALTH_TIMEOUT  seconds to wait for /health (default 5)
#
# Exit status: 0 means the runtime answered and the ping was delivered. Non-zero
# means the runtime did not answer — in which case no ping is sent, deliberately,
# because not pinging is what makes the switch fire — or the ping itself could
# not be delivered. Run it from a systemd timer; see heartbeat.service and
# heartbeat.timer beside this script.
#
# /health answers without a token by design (internal/api/server.go), so this
# needs no credential and discloses nothing beyond status, version and uptime.
set -eu

api=${OTTER_API_URL:-http://127.0.0.1:7337}
url=${OTTER_HEARTBEAT_URL:?set OTTER_HEARTBEAT_URL to the dead-man switch to ping}
timeout=${OTTER_HEALTH_TIMEOUT:-5}

if ! curl -fsS --max-time "$timeout" "$api/health" >/dev/null 2>&1; then
	echo "heartbeat: $api/health did not answer; not pinging, so the switch fires" >&2
	exit 1
fi

# The runtime is up: say so. A delivery failure is a real failure rather than
# something to swallow — a switch that is not told this host is alive will fire,
# which is the correct alarm for a heartbeat that could not be delivered.
curl -fsS --max-time "$timeout" "$url" >/dev/null
