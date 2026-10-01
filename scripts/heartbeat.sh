#!/bin/sh
# Dead-man's-switch heartbeat for otterd.
#
# A positive check — "curl /health and alert if it fails" — is not enough. If the
# checker itself dies, the host loses egress, or the cron entry is removed,
# nobody finds out, because the thing that would have reported the problem is the
# thing that is gone. A dead-man's switch inverts it: this script publishes a
# Heartbeat metric only while the runtime answers, so *silence* is the alarm, and
# it still arrives when this host can no longer send anything at all. The alarm
# lives in the CloudWatch stack that reads the `Otter` namespace, not here.
#
#   OTTER_API_URL         the daemon's API (default http://127.0.0.1:7337)
#   OTTER_HEALTH_TIMEOUT  seconds to wait for /health (default 5)
#
# The metric itself is published by otter-metric.sh, installed beside this
# script in /usr/local/lib/otter/. It takes the Host dimension from
# OTTER_METRIC_HOST and its credentials from the instance role through IMDSv2;
# see that script's header. There is no URL here to configure any more.
#
# Exit status: 0 means the runtime answered and the metric was accepted. Non-zero
# means the runtime did not answer — in which case nothing is published,
# deliberately, because the missing datapoint is what makes the alarm fire — or
# the publish itself failed. Run it from a systemd timer; see heartbeat.service
# and heartbeat.timer beside this script.
#
# /health answers without a token by design (internal/api/server.go), so this
# needs no credential and discloses nothing beyond status, version and uptime.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
api=${OTTER_API_URL:-http://127.0.0.1:7337}
timeout=${OTTER_HEALTH_TIMEOUT:-5}

if ! curl -fsS --max-time "$timeout" "$api/health" >/dev/null 2>&1; then
	echo "heartbeat: $api/health did not answer; publishing nothing, so the alarm fires" >&2
	exit 1
fi

# The runtime is up: say so. A publish failure is a real failure rather than
# something to swallow — a switch that is not told this host is alive will fire,
# which is the correct alarm for a heartbeat that could not be delivered.
sh "$here/otter-metric.sh" Heartbeat=1
