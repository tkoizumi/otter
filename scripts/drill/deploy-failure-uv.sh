#!/bin/sh
# Minimal uv stand-in for the P0-06 deployment-failure drill.
#
# `otter deploy` vendors the real uv, which downloads or installs a managed
# CPython and resolves the lockfile. This drill needs neither: it is about what
# happens when preparation FAILS after a release has been staged, not about
# Python packaging. The drill pre-seeds the local uv cache that `otter deploy`
# reads with this script, so the ordinary vendoring path pushes it to the host
# as <workspace>/tools/uv/uv and both `otter release` and otterd resolve it the
# way they resolve a real uv. No network, no interpreter download.
#
# It implements exactly the surface pyenv.Manager touches, and nothing else:
#
#   uv --version                      the version string that is part of a
#                                     prepared environment's identity
#   uv python install --install-dir D makes D/<version>/bin/python the host's
#                                     python3, so no download happens
#   uv sync ...                       writes the venv config the interpreter
#                                     needs; the interpreter itself keeps
#                                     working without site-packages
#
# Every other subcommand exits non-zero, so a step this stub does not model is
# a loud failure rather than a silent success.
#
# Injection hook. Two switches make preparation fail:
#
#   the file /etc/otter/drill-preparation-fail   (the drill's injection)
#   DRILL_UV_FAIL=1                              (manual)
#
# The drill creates the file on the host before the candidate deploy. It is a
# file rather than an environment variable because `otter deploy` re-pushes
# this script and runs `otter release` itself, so the trigger has to live
# somewhere the deploy does not reset. `--version` must keep working while the
# hook is set: the version string is part of the environment identity, and the
# candidate has to be staged under its real digest before preparation is
# refused -- that midpoint is the whole point of the drill.
set -eu

FAIL_MARKER=${DRILL_UV_FAIL_MARKER:-/etc/otter/drill-preparation-fail}

fail_if_marked() {
	if [ "${DRILL_UV_FAIL:-}" = "1" ] || [ -e "$FAIL_MARKER" ]; then
		echo "minimal-uv: preparation is injected to fail ($FAIL_MARKER)" >&2
		exit 1
	fi
}

# link_python puts a runnable interpreter at dest. A hard link is used because
# CPython resolves a symlinked argv[0] to the real binary, and the runtime
# executes <env>/bin/python directly, so a symlink that lands on python3.12 is
# a path the runtime then cannot find.
link_python() {
	src=$1
	dest=$2
	rm -f "$dest"
	if ln "$src" "$dest" 2>/dev/null; then
		return 0
	fi
	cp "$src" "$dest"
}

case "${1:-}" in
--version)
	echo "uv 0.0.0-drill"
	exit 0
	;;
python)
	[ "${2:-}" = "install" ] || {
		echo "minimal-uv: unsupported: python ${2:-}" >&2
		exit 1
	}
	fail_if_marked
	shift 2
	install_dir=
	pin=
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--install-dir)
			install_dir=$2
			shift 2
			;;
		*)
			pin=$1
			shift
			;;
		esac
	done
	[ -n "$install_dir" ] && [ -n "$pin" ] || {
		echo "minimal-uv: usage: uv python install --install-dir DIR PIN" >&2
		exit 1
	}
	host_python=$(command -v python3 || true)
	[ -n "$host_python" ] || {
		echo "minimal-uv: no python3 on PATH" >&2
		exit 1
	}
	mkdir -p "$install_dir/$pin/bin"
	# Both steps create the interpreter. The install step records it where uv
	# would have put the managed toolchain, and sync recreates it inside the
	# project environment because pyenv.Manager wipes that directory between
	# install and sync, which would delete anything left here.
	link_python "$host_python" "$install_dir/$pin/bin/python"
	echo "minimal-uv: $pin -> $host_python (no download)"
	;;
sync)
	fail_if_marked
	# pyenv.Manager.Prepare passes UV_PROJECT_ENVIRONMENT in the environment and
	# does not put --project-environment on the command line; read both so the
	# stub keeps working if that ever changes.
	projenv=${UV_PROJECT_ENVIRONMENT:-}
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--python | --python-preference | --project-environment | --directory)
			if [ "$1" = "--project-environment" ]; then
				projenv=$2
			fi
			shift 2
			;;
		*)
			shift
			;;
		esac
	done
	[ -n "$projenv" ] || {
		echo "minimal-uv: sync without UV_PROJECT_ENVIRONMENT" >&2
		exit 1
	}
	py=$(command -v python3 || true)
	[ -n "$py" ] || exit 1
	mkdir -p "$projenv/bin"
	link_python "$py" "$projenv/bin/python"
	{
		echo "home = $(dirname "$(dirname "$py")")"
		echo "include-system-site-packages = false"
		echo "version = $(python3 -c 'import sys; print(".".join(map(str, sys.version_info[:3])))')"
	} >"$projenv/pyvenv.cfg"
	echo "minimal-uv: synced $projenv (no dependencies)"
	;;
*)
	echo "minimal-uv: unsupported subcommand: ${1:-}" >&2
	exit 1
	;;
esac
