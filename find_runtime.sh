#!/bin/sh
# Every Otter runtime on this machine, with the workspace and state it owns.
#
# A healthy machine shows one line per workspace. A stray runtime is one whose
# workspace directory no longer exists, or whose path is under /tmp.
#
# The daemons run as `otter start ...`, not `otterd`, so matching the process
# name alone finds nothing -- the command line is what identifies them, and it
# already contains the workspace, the data directory and the port.

# pgrep -fl prints "<pid> <full command line>". Filtering on the executable in
# that line keeps this script and the pgrep itself out of the results.
lines=$(pgrep -fl 'otter' 2>/dev/null | grep -E '/otter(d)?( |$)')

if [ -z "$lines" ]; then
    echo "no Otter runtime is running"
    exit 0
fi

printf '%s\n' "$lines" | while IFS= read -r line; do
    pid=${line%% *}
    cmd=${line#* }

    ws=$(printf '%s\n' "$cmd" | sed -n 's/.*--integrations \([^ ]*\).*/\1/p')
    data=$(printf '%s\n' "$cmd" | sed -n 's/.*--data \([^ ]*\).*/\1/p')
    listen=$(printf '%s\n' "$cmd" | sed -n 's/.*--listen \([^ ]*\).*/\1/p')

    # The database the daemon actually holds open: the proof it owns the state
    # it claims, rather than having recorded a path that moved underneath it.
    db=$(lsof -p "$pid" 2>/dev/null | grep -m1 'otter\.db$' | awk '{print $NF}')

    printf '\npid %s\n' "$pid"
    printf '  workspace  %s\n' "${ws:-unknown}"
    printf '  data       %s\n' "${data:-unknown}"
    printf '  open db    %s\n' "${db:-unknown}"
    printf '  api        http://%s\n' "${listen:-unknown}"

    if [ -n "$ws" ] && [ ! -d "$ws" ]; then
        printf '  WARNING    workspace no longer exists: this is a stray runtime\n'
    fi
    case "$ws" in
        /tmp/*|/private/tmp/*)
            printf '  WARNING    workspace is under /tmp: almost certainly a leftover\n' ;;
    esac
done
