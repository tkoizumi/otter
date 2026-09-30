#!/bin/sh
# run-otter.sh -- run the Otter CLI on a host with the workspace's API token,
# read there from the environment file the daemon itself loads.
#
# Why this exists. `otter deploy` writes the API token to
# `/etc/otter/workspaces/<workspace>.env`, but the CLI's own discovery globs
# only `/etc/otter/*.env` -- `*` does not cross `workspaces/`. So on a normally
# deployed host, every CLI call a remote operator makes starts with a 401, and
# the clean-host drill would report that as "the runtime does not know this
# job": a misleading red that costs an afternoon.
#
# The drill therefore resolves the token FILE on the host (see
# lib/host-report.sh) and runs the CLI through this script, which reads the
# token inside the remote shell and hands it to the CLI in its environment.
# The secret never appears in a command line -- `argv` is readable by every
# process on the host -- and never in the drill's transcript.
#
# Usage:
#   run-otter.sh <token_file|-> <api_url> <otter_bin> [args...]
#
#   token_file  the file to read OTTER_API_TOKEN from, or `-` when the daemon
#               needs no token (an empty value is treated as unset by the CLI).
#   api_url     loopback API base URL of the daemon on this host.
#
# Exits with the CLI's own status.
set -eu

[ "$#" -ge 3 ] || {
	echo "usage: run-otter.sh <token_file|-> <api_url> <otter_bin> [args...]" >&2
	exit 2
}

token_file=$1
api_url=$2
bin=$3
shift 3

if [ "$token_file" != "-" ] && [ -f "$token_file" ]; then
	OTTER_API_TOKEN=$(sed -n 's/^OTTER_API_TOKEN=//p' "$token_file" | head -n 1)
	export OTTER_API_TOKEN
fi
OTTER_API_URL=$api_url
export OTTER_API_URL

exec "$bin" "$@"
