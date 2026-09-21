#!/bin/sh
# Contributor smoke test: run the shipped product end to end from a workspace
# outside this checkout.
#
# The canonical sample is `otter init`'s output. That is what makes this worth
# having: the scaffold users get is the scaffold this repository tests, so it
# cannot drift from the docs the way a checked-in example can.
#
# Nothing here reads this checkout's configuration or talks to a runtime that
# is already running. The workspace is a fresh temporary directory, the binary
# is the one this checkout just built, and the API URL is the one that
# workspace records for itself.
set -eu

OTTER_BIN=${OTTER_BIN:?OTTER_BIN must be an absolute path to the otter binary}
case "$OTTER_BIN" in
	/*) ;;
	*) echo "smoke: OTTER_BIN must be absolute, got $OTTER_BIN" >&2; exit 2 ;;
esac
[ -x "$OTTER_BIN" ] || { echo "smoke: not executable: $OTTER_BIN" >&2; exit 2; }

# A developer's shell must not redirect the run at another daemon.
unset OTTER_API_URL OTTER_API_TOKEN 2>/dev/null || true

NAME=smoke
WORK=$(mktemp -d "${TMPDIR:-/tmp}/otter-smoke.XXXXXX")
cleaning=0

cleanup() {
	status=$?
	[ "$cleaning" -eq 1 ] && return
	cleaning=1

	# Stop only the runtime this workspace recorded. A failure to stop must not
	# mask the reason the test failed.
	( cd "$WORK" 2>/dev/null && "$OTTER_BIN" stop >/dev/null 2>&1 ) || true

	if [ "$status" -ne 0 ]; then
		echo "smoke: failed; last daemon log lines:" >&2
		tail -n 30 "$WORK/.otter/serve/serve.log" >&2 2>/dev/null || true
		echo "smoke: workspace was $WORK" >&2
	fi

	# Keep the workspace for a human debugging locally; CI gets the log above
	# and nothing left behind.
	if [ -n "${OTTER_SMOKE_KEEP:-}" ] || { [ "$status" -ne 0 ] && [ -z "${CI:-}" ]; }; then
		echo "smoke: keeping $WORK" >&2
	else
		rm -rf "$WORK"
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM

cd "$WORK"

echo "smoke: init"
"$OTTER_BIN" init "$NAME" >/dev/null
"$OTTER_BIN" validate "$NAME" >/dev/null

echo "smoke: release"
"$OTTER_BIN" release "$NAME" >/dev/null

echo "smoke: start"
"$OTTER_BIN" start --detach >/dev/null

# Ready means the API answers for this workspace, not merely that a process
# exists.
i=0
until "$OTTER_BIN" status >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -lt 150 ] || { echo "smoke: runtime did not become ready" >&2; exit 1; }
	sleep 0.2
done

echo "smoke: run"
"$OTTER_BIN" run "$NAME" >/dev/null

# With stdout not a terminal `otter run` queues and returns, so success is the
# state the run persists -- read back through the same CLI an operator uses.
i=0
count=""
until count=$("$OTTER_BIN" state get "$NAME" count 2>/dev/null); do
	i=$((i + 1))
	[ "$i" -lt 150 ] || { echo "smoke: the run never persisted its state" >&2; exit 1; }
	sleep 0.2
done
[ "$count" = "1" ] || { echo "smoke: state count=$count, want 1" >&2; exit 1; }

echo "smoke: ok (count=$count)"
