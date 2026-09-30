#!/bin/sh
# remote-args.sh -- quote an argument list so it survives being sent to a
# remote shell.
#
# Why this is its own file, and why it is not `$*`. The drill builds remote
# commands as `ssh host "sh -s -- <args>"`. `$*` joins the arguments with a
# space, and the remote shell then splits that string on whitespace -- so an
# EMPTY argument disappears. That is not cosmetic:
#
#   * the clean-host probe takes nine parameters (role, data dir, jobs root,
#     CLI, api url, unit, api token, env file, etc dir). With the documented
#     invocation the token and env-file parameters are empty, so `/etc/otter`
#     slid into the token slot and the token gate refused every host;
#   * hot-backup.sh requires exactly five parameters. A clean run passes an
#     empty sabotage, so the backup was called with four and exited 2 with a
#     usage message: the clean path could not run at all.
#
# Quoting each parameter separately keeps it, including as the empty string.
# Values that reach this function are validated against a charset that excludes
# quotes; the escape below is belt to that braces.
#
# Usage:  args=$(remote_args "$a" "$b" ...)      # then:  ... sh -s --$args
remote_args() {
	remote_args_out=""
	for remote_args_arg in "$@"; do
		remote_args_escaped=$(printf '%s' "$remote_args_arg" | sed "s/'/'\\\\''/g")
		remote_args_out="$remote_args_out '$remote_args_escaped'"
	done
	printf '%s' "$remote_args_out"
}
