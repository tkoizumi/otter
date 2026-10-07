#!/bin/sh
# Falsifiability for monitoring-contract.json: the single home for the four
# CloudWatch strings the runtime publishes and the alarm stack watches.
#
# The runtime's publisher hardcodes the namespace and the Host dimension and
# takes the metric names as arguments from heartbeat.sh and disk-check.sh;
# otter-platform's CDK declares all four independently. They agree today, but
# nothing tied them: a rename on either side would leave the alarm firing
# forever (or never) with no test saying why (OT-024).
#
# This suite checks the producer half against the contract file. The consumer
# half is checked in otter-platform/runtime/test/castor-runtime-stack.test.ts,
# which reads the same file. The suite proves it can fail: it mutates a copy of
# the publisher and requires the check to go red on it.
#
#   sh scripts/test-monitoring-contract.sh
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
contract=${OTTER_MONITORING_CONTRACT:-$here/../monitoring-contract.json}
publisher=${OTTER_METRIC_SUBJECT:-$here/otter-metric.sh}
heartbeat=${OTTER_HEARTBEAT_SUBJECT:-$here/heartbeat.sh}
disk=${OTTER_DISK_SUBJECT:-$here/disk-check.sh}

[ -f "$contract" ] || {
	echo "test-monitoring-contract: missing $contract" >&2
	exit 1
}
for f in "$publisher" "$heartbeat" "$disk"; do
	[ -f "$f" ] || {
		echo "test-monitoring-contract: missing $f" >&2
		exit 1
	}
done

# The contract is JSON. python3 is already a hard dependency of the runtime's
# suites and is pinned in CI, so it is the parser here rather than an assumed jq.
py=${OTTER_PYTHON:-python3}
command -v "$py" >/dev/null 2>&1 || {
	echo "test-monitoring-contract: $py is required to read the contract" >&2
	exit 1
}

field() {
	"$py" -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$contract" "$1"
}

namespace=$(field namespace)
dimension=$(field dimension)
tenant_dimension=$(field tenant_dimension)
heartbeat_name=$("$py" -c 'import json,sys; print(json.load(open(sys.argv[1]))["metrics"]["heartbeat"]["name"])' "$contract")
disk_name=$("$py" -c 'import json,sys; print(json.load(open(sys.argv[1]))["metrics"]["disk_check"]["name"])' "$contract")

passed=0
failed=0

pass() { passed=$((passed + 1)); echo "ok: $1"; }
fail() { failed=$((failed + 1)); echo "FAIL: $1" >&2; }

# check runs every assertion against one set of files, so the mutation case and
# the real case share exactly the same logic.
check() {
	pub=$1
	hb=$2
	dk=$3
	label=$4

	# Anchored on the closing quote: an unanchored grep for Namespace=Otter is
	# satisfied by Namespace=OtterMetricsX, which is how a rename would ship
	# green (the same false-green OT-027 found in the old publisher suite).
	if grep -q "Namespace=$namespace\"" "$pub"; then
		pass "$label: publisher sends Namespace=$namespace"
	else
		fail "$label: publisher does not send Namespace=$namespace"
	fi
	if grep -q "Dimensions.member.1.Name=$dimension\"" "$pub"; then
		pass "$label: publisher sends the $dimension dimension"
	else
		fail "$label: publisher does not send the $dimension dimension"
	fi
	if grep -q "Dimensions.member.2.Name=$tenant_dimension\"" "$pub"; then
		pass "$label: publisher can send the $tenant_dimension dimension"
	else
		fail "$label: publisher cannot send the $tenant_dimension dimension"
	fi
	if grep -q "otter-metric.sh\" $heartbeat_name=" "$hb"; then
		pass "$label: heartbeat publishes $heartbeat_name"
	else
		fail "$label: heartbeat does not publish $heartbeat_name"
	fi
	if grep -q "otter-metric.sh\" $disk_name=" "$dk"; then
		pass "$label: disk-check publishes $disk_name"
	else
		fail "$label: disk-check does not publish $disk_name"
	fi
	if grep -q "MetricData.member.\$member.MetricName=" "$pub"; then
		pass "$label: publisher carries the metric name it was given"
	else
		fail "$label: publisher does not carry a MetricName parameter"
	fi
}

check "$publisher" "$heartbeat" "$disk" "contract"

# Falsifiability: mutate the namespace in a copy of the publisher and require
# the same check to notice. A check that cannot go red is not a check. The
# probe's own tally is discarded -- it is a meta-check, not a contract check.
before_passed=$passed
before_failed=$failed
tmp=$(mktemp -d "${TMPDIR:-/tmp}/otter-contract.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
sed "s/Namespace=$namespace/Namespace=${namespace}MetricsX/" "$publisher" >"$tmp/otter-metric.sh"
check "$tmp/otter-metric.sh" "$heartbeat" "$disk" "mutated" >/dev/null 2>&1
mutated_red=0
if [ "$failed" -gt "$before_failed" ]; then
	mutated_red=1
fi
passed=$before_passed
failed=$before_failed
if [ "$mutated_red" = 1 ]; then
	pass "a renamed namespace turns the contract check red"
else
	fail "the contract check stayed green on a renamed namespace"
fi

echo ""
if [ "$failed" -gt 0 ]; then
	echo "test-monitoring-contract: FAILED ($failed check(s))" >&2
	exit 1
fi
echo "test-monitoring-contract: all contract checks passed"
