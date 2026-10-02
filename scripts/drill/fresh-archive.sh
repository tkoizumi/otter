#!/bin/sh
# Drill: a release archive installs and runs a job on a host that has no
# checkout (CA-24, R-14).
#
# Why this exists: `scripts/smoke.sh` proves init -> validate -> release -> run ->
# state -> stop, but it does so with a binary this checkout just built. The gate
# is stronger than that: what a stranger downloads must work on its own. This
# drill takes a *release archive*, extracts it outside the checkout, and drives
# the whole loop with the extracted binary, so nothing in the path under test
# comes from the source tree.
#
# What it does:
#   1. requires OTTER_ARCHIVE, a release archive named
#      otter_<version>_<os>_<arch>.tar.gz, and refuses anything else;
#   2. extracts it into a temporary directory outside the checkout and asserts
#      it carries both published binaries, executable;
#   3. asserts `otter --version` runs, and that a tag-style version in the
#      archive's name is the version the binary reports (a snapshot version such
#      as `0.3.1-next` is reported and not asserted);
#   4. runs the shipped smoke loop -- init, validate, release, start, run, read
#      state, stop -- with OTTER_BIN pointing at the extracted binary, in a
#      temporary workspace outside the checkout.
#
# What it proves:
#   * the archive is self-contained: both binaries extract and run, and no
#     checkout, `go`, `make` or repo path is needed to execute a job;
#   * the scaffold a first-time user gets from the archive is a working job.
#
# What it explicitly does NOT prove:
#   * macOS notarization or install.sh/Homebrew cask behavior. Those are the
#     release pipeline's own steps (`.github/workflows/release.yml`,
#     `release-config` job);
#   * a restore onto a fresh host. That is `backup-restore`'s clean-host mode.
#
# Running it: `OTTER_ARCHIVE=dist/otter_0.3.0_linux_amd64.tar.gz make drill
# DRILL=fresh-archive`. CI does this in the `release-config` job against the
# snapshot archives, which is the only place the release layout is built.
#
# Falsifiability (DRILL_SABOTAGE). The clean run must be green. With
# `DRILL_SABOTAGE=stale-binary` the loop is driven by this checkout's own
# `bin/otter` instead of the extracted one, so the "the runtime under test is the
# archive" assertion goes red -- the drill detects a checkout binary being passed
# off as an installed release.
set -eu

OTTER_ARCHIVE=${OTTER_ARCHIVE:?OTTER_ARCHIVE must be an absolute path to otter_<version>_<os>_<arch>.tar.gz (produce one with: goreleaser release --snapshot --clean)}
case "$OTTER_ARCHIVE" in
/*) ;;
*)
	echo "drill: OTTER_ARCHIVE must be an absolute path, got $OTTER_ARCHIVE" >&2
	exit 2
	;;
esac
[ -f "$OTTER_ARCHIVE" ] || {
	echo "drill: no such archive: $OTTER_ARCHIVE" >&2
	exit 2
}

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
SABOTAGE=${DRILL_SABOTAGE:-}
case "$SABOTAGE" in
"" | stale-binary) ;;
*)
	echo "drill: unknown DRILL_SABOTAGE=$SABOTAGE" >&2
	echo "drill: valid modes: stale-binary" >&2
	exit 2
	;;
esac

NAME=$(basename "$OTTER_ARCHIVE")
case "$NAME" in
otter_*.tar.gz) ;;
*)
	echo "drill: $NAME is not a release archive (want otter_<version>_<os>_<arch>.tar.gz)" >&2
	exit 2
	;;
esac
ARCHIVE_VERSION=$(printf '%s' "$NAME" | sed 's/^otter_//; s/_.*$//')
[ -n "$ARCHIVE_VERSION" ] || {
	echo "drill: cannot read a version out of $NAME" >&2
	exit 2
}

tmpbase=${TMPDIR:-/tmp}
while [ "$tmpbase" != "${tmpbase%/}" ]; do tmpbase=${tmpbase%/}; done
[ -n "$tmpbase" ] || tmpbase=/tmp
WORK=$(mktemp -d "$tmpbase/otter-drill-fresh-archive.XXXXXX")
INSTALL="$WORK/install"
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

	# The smoke loop stops the runtime it started; this drill owns no workspace
	# of its own to stop, and the extraction directory is removed below.

	echo "drill: elapsed $(( $(date +%s) - START_EPOCH ))s" >&2
	if [ -n "${OTTER_DRILL_KEEP:-}" ] || { [ "$status" -ne 0 ] && [ -z "${CI:-}" ]; }; then
		echo "drill: keeping $WORK" >&2
	else
		rm -rf "$WORK"
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM

# 1. extract outside the checkout.
mkdir -p "$INSTALL"
say "extract     $NAME"
tar -xzf "$OTTER_ARCHIVE" -C "$INSTALL" || fail "the archive did not extract"
[ -x "$INSTALL/otter" ] || fail "the archive carries no executable otter"
[ -x "$INSTALL/otterd" ] || fail "the archive carries no executable otterd"
say "members     otter, otterd"

# 2. the extracted binary runs, and says which version it is.
VERSION=$("$INSTALL/otter" --version 2>&1) ||
	fail "the extracted otter does not run (is the archive for another platform?): $VERSION"
say "version     $VERSION"
case "$ARCHIVE_VERSION" in
*[!0-9.]*)
	say "note: '$ARCHIVE_VERSION' is a snapshot version; not asserting the binary reports it"
	;;
*)
	printf '%s' "$VERSION" | grep -q "$ARCHIVE_VERSION" ||
		fail "the archive is named $ARCHIVE_VERSION but the binary reports '$VERSION'"
	;;
esac

# 3. the loop, driven by the installed binary only.
RUNTIME_BIN="$INSTALL/otter"
case "$SABOTAGE" in
stale-binary)
	RUNTIME_BIN="$ROOT/bin/otter"
	say "sabotage: driving the loop with this checkout's bin/otter"
	[ "$RUNTIME_BIN" != "$INSTALL/otter" ] ||
		fail "the sabotage did not select a checkout binary"
	;;
esac
case "$RUNTIME_BIN" in
"$WORK"/*) ;;
*)
	fail "the runtime under test is not from the archive ($RUNTIME_BIN)"
	;;
esac

say "smoke       init -> validate -> release -> start -> run -> state -> stop"
OTTER_BIN="$RUNTIME_BIN" sh "$ROOT/scripts/smoke.sh" ||
	fail "the shipped smoke loop failed against the archive"
say "ok: $NAME installed outside the checkout and ran a job end to end"
