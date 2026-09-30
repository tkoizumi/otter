#!/bin/sh
# Provision a fresh Ubuntu 24.04 host for Otter and converge it with `otter
# deploy`.
#
# This is the first half of the P0-07 deliverable. The boundary is deliberate and
# narrow:
#
#   * this script owns everything *before* the runtime exists on the host -- the
#     host's own preconditions (packages, architecture, RAM, disk, swap,
#     restricted SSH, sudo) and the assertion that the host is fresh rather than
#     half-provisioned;
#   * `otter deploy` owns everything after that -- the service account with a
#     `nologin` shell, the data and env directories with their modes, the
#     systemd unit, the release, and the post-deploy /health poll. This script
#     does not recreate any of it, and re-running `otter deploy` remains the way
#     to converge a host;
#   * `scripts/assert-host-permissions.sh` then checks, on the host, that what
#     the deploy created is actually there -- modes, ownership, the unit's
#     sandbox directives, loopback-only ingress and the approved ports, plus
#     swap and the provisioning report. A deploy that reports success but left
#     the host unsafe fails here.
#
# It runs on the workstation, from the checkout being deployed; the host is
# reached over SSH. It never runs anything against a host that has not been
# explicitly named, and `--dry-run` prints the plan without executing it.
#
#   sh scripts/provision.sh --host ubuntu@203.0.113.10
#   sh scripts/provision.sh --host ubuntu@203.0.113.10 --identity ~/.ssh/castor
#   sh scripts/provision.sh --host ubuntu@203.0.113.10 --dry-run
#
# Why the swap check is a hard failure and not a warning: a cloud-init user-data
# script on the real host ran under `set -euo pipefail` and executed
# `systemctl reload ssh || systemctl reload sshd`. Ubuntu 24.04's sshd is
# socket-activated, so neither unit is reloadable, both sides of that `||`
# failed, the script aborted before its swapfile block, and the host came up
# with zero swap -- while `cloud-init status` reported an error that nothing
# read. State a host is supposed to have must be asserted, not created
# best-effort in the middle of a script. See docs/operations.md.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/.." && pwd)
# OTTER_ASSERT_SCRIPT exists so scripts/test-provision.sh can point at the real
# assertion script while running a mutated copy of this one from elsewhere. It
# is resolved here rather than with ${VAR:-default}, because under `set -u` the
# default in the same assignment is not reliably visible yet.
assert_script=${OTTER_ASSERT_SCRIPT:-}
[ -n "$assert_script" ] || assert_script=$here/assert-host-permissions.sh

usage() {
	cat <<'USAGE'
usage: provision.sh --host USER@HOST [options]

Provision a fresh Ubuntu 24.04 host for Otter: assert its preconditions, run
`otter deploy`, then assert the resulting permissions and ingress on the host.

  --host USER@HOST        SSH destination (required)
  --user USER             SSH login user, when not embedded in --host
  --port N                SSH port (default 22)
  --identity FILE         private key for SSH and rsync
  --project DIR           project directory to deploy (default: the checkout root)
  --daemon-env FILE       daemon environment file to upload (default
                          <project>/otter.daemon.env; a template is
                          otter.daemon.env.example)
  --workers N             OTTER_WORKERS injected into the deployment (default 1)
  --otter-bin PATH        otter CLI to run (default: <checkout>/bin/otter, built
                          if missing and make is available)
  --remote-dir DIR        install root on the host (default /opt/otter)
  --service-user USER     service account (default otter)
  --platform GOOS/GOARCH  target platform; detected on the host when empty
  --min-ram-mib N         minimum MemTotal (default 800; the 1 GiB Castor host
                          reports 904 MiB)
  --min-disk-mib N        minimum free space on the install root (default 2048)
  --min-swap-mib N        minimum active swap (default 256)
  --allow-existing        do not fail when the host is already provisioned
                          (the deploy is a converge)
  --dry-run               print every command instead of running it
  -h, --help              this text
USAGE
}

host=${OTTER_PROVISION_HOST:-}
ssh_user=${OTTER_PROVISION_USER:-}
ssh_port=${OTTER_PROVISION_PORT:-22}
identity=${OTTER_PROVISION_IDENTITY:-}
project=${OTTER_PROVISION_PROJECT:-$root}
daemon_env=${OTTER_DAEMON_ENV:-}
workers=${OTTER_WORKERS:-1}
otter_bin=${OTTER_BIN:-}
remote_dir=/opt/otter
service_user=otter
platform=${OTTER_PLATFORM:-}
min_ram_mib=${OTTER_MIN_RAM_MIB:-800}
min_disk_mib=${OTTER_MIN_DISK_MIB:-2048}
min_swap_mib=${OTTER_MIN_SWAP_MIB:-256}
allow_existing=0
dry_run=0

while [ $# -gt 0 ]; do
	case $1 in
	--host)
		host=$2
		shift 2
		;;
	--user)
		ssh_user=$2
		shift 2
		;;
	--port)
		ssh_port=$2
		shift 2
		;;
	--identity)
		identity=$2
		shift 2
		;;
	--project)
		project=$2
		shift 2
		;;
	--daemon-env)
		daemon_env=$2
		shift 2
		;;
	--workers)
		workers=$2
		shift 2
		;;
	--otter-bin)
		otter_bin=$2
		shift 2
		;;
	--remote-dir)
		remote_dir=$2
		shift 2
		;;
	--service-user)
		service_user=$2
		shift 2
		;;
	--platform)
		platform=$2
		shift 2
		;;
	--min-ram-mib)
		min_ram_mib=$2
		shift 2
		;;
	--min-disk-mib)
		min_disk_mib=$2
		shift 2
		;;
	--min-swap-mib)
		min_swap_mib=$2
		shift 2
		;;
	--allow-existing)
		allow_existing=1
		shift
		;;
	--dry-run)
		dry_run=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "provision: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

if [ -z "$host" ]; then
	echo "provision: --host is required, for example --host ubuntu@203.0.113.10" >&2
	exit 2
fi
[ -n "$ssh_user" ] || ssh_user=ubuntu
destination=$ssh_user@${host#*@}

# --- output -----------------------------------------------------------------

say() {
	printf 'provision: %s\n' "$1"
}

die() {
	printf 'provision: FAIL: %s\n' "$1" >&2
	exit 1
}

# quote single-quotes a string for a POSIX shell. Every remote command is built
# through it, so a path or user name with a space cannot turn into two commands.
quote() {
	printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

# A transcript is only evidence if it says where and when it ran, against which
# checkout. Same header contract as scripts/drill.sh.
printf '\n===== provision =====\n'
printf 'date:        %s\n' "$(date '+%Y-%m-%d %H:%M:%S %Z')"
printf 'host:        %s %s (%s)\n' "$(uname -s)" "$(uname -r)" "$(uname -m)"
printf 'checkout:    %s\n' "$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo unknown)"
printf 'destination: %s\n' "$destination"
printf 'project:     %s\n' "$project"
printf 'remote dir:  %s\n' "$remote_dir"
printf 'dry run:     %s\n' "$dry_run"

if [ ! -f "$assert_script" ]; then
	echo "provision: FAIL: missing $assert_script" >&2
	exit 2
fi

# --- ssh plumbing -----------------------------------------------------------
#
# Every host interaction goes through `remote`, so --dry-run can print the
# command instead of running it and the transcript is the complete list of
# things the script would do.

ssh_args="-o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15"
ssh_args="$ssh_args -p $ssh_port"
[ -z "$identity" ] || ssh_args="$ssh_args -i $identity"

remote() {
	# shellcheck disable=SC2086
	ssh $ssh_args "$destination" "$1"
}

# mib converts a KiB count read from the host into MiB, defaulting to 0 for
# anything unparsable. Arithmetic on an empty string is a syntax error under
# `set -e`, which would abort the script with a shell message instead of the
# named failure the operator needs.
mib() {
	value=$(printf '%s' "${1:-0}" | tr -cd '0-9')
	printf '%s' "$(( ${value:-0} / 1024 ))"
}

# --- the daemon environment -------------------------------------------------
#
# OTTER_WORKERS=1 is not a preference on a 1 GiB host. The daemon defaults to
# one worker per vCPU (2 on the Castor instance) and MemoryMax=75% bounds the
# whole unit cgroup at roughly 680 MiB, so two Python processes plus the daemon
# is how the host OOMs. The script writes the file when the project has none, so
# the safe value is the default rather than a step an operator can forget.

if [ -z "$daemon_env" ]; then
	daemon_env=$project/otter.daemon.env
fi
# Absolute, because the deploy runs from the project directory and this file may
# live in a temporary directory beside it.
case $daemon_env in
/*) ;;
*)
	daemon_env=$(CDPATH= cd -- "$(dirname -- "$daemon_env")" && pwd)/$(basename -- "$daemon_env")
	;;
esac

if [ ! -f "$daemon_env" ]; then
	case $daemon_env in
	"$project"/*) ;;
	*)
		echo "provision: FAIL: $daemon_env does not exist" >&2
		exit 2
		;;
	esac
	if [ "$dry_run" -eq 1 ]; then
		say "dry-run: $daemon_env does not exist; would write it with OTTER_WORKERS=$workers"
	else
		printf 'OTTER_WORKERS=%s\n' "$workers" >"$daemon_env"
		say "wrote $daemon_env (OTTER_WORKERS=$workers); edit it to add daemon settings"
	fi
fi

daemon_env_deploy=$daemon_env
if [ -f "$daemon_env" ] && grep -q '^[[:space:]]*OTTER_WORKERS=' "$daemon_env" 2>/dev/null; then
	# An operator who sized the host deliberately wins over the default.
	workers=$(sed -n 's/^[[:space:]]*OTTER_WORKERS=//p' "$daemon_env" | head -n 1)
	say "using OTTER_WORKERS=$workers from $daemon_env"
elif [ "$dry_run" -eq 1 ]; then
	say "dry-run: would inject OTTER_WORKERS=$workers into the deployed daemon environment"
else
	daemon_env_deploy=$(mktemp "${TMPDIR:-/tmp}/otter-daemon-env.XXXXXX")
	{
		printf 'OTTER_WORKERS=%s\n' "$workers"
		cat "$daemon_env"
	} >"$daemon_env_deploy"
	say "injecting OTTER_WORKERS=$workers into the deployed daemon environment"
fi

# --- the otter CLI ----------------------------------------------------------

if [ -z "$otter_bin" ]; then
	otter_bin=$root/bin/otter
fi
if [ ! -x "$otter_bin" ]; then
	if [ "$dry_run" -eq 1 ]; then
		say "dry-run: $otter_bin is not executable; would build it with make build"
	elif command -v make >/dev/null 2>&1; then
		say "building $otter_bin"
		(cd "$root" && make build)
	else
		echo "provision: FAIL: $otter_bin is not executable and make is not on PATH" >&2
		exit 2
	fi
fi

# --- 1. the host answers, and sudo works ------------------------------------

say "checking $destination is reachable and has passwordless sudo"
login_uid=0
remote_arch=""
if [ "$dry_run" -eq 1 ]; then
	say "dry-run: would run: id -u; sudo -n true; uname -s; uname -m"
	remote_arch=${OTTER_DRY_RUN_ARCH:-aarch64}
else
	login_uid=$(remote 'id -u' 2>&1) || die "$destination is not reachable over SSH: $login_uid"
	login_uid=$(printf '%s' "$login_uid" | tr -d '\r')
	[ -n "$login_uid" ] || die "$destination returned nothing for 'id -u'"
	if [ "$login_uid" != "0" ]; then
		remote 'sudo -n true' >/dev/null 2>&1 ||
			die "the login $ssh_user is not root and has no passwordless sudo; systemd needs root"
	fi
	platform_remote=$(remote 'uname -s && uname -m' | tr -d '\r') ||
		die "could not run uname on $destination"
	remote_sys=${platform_remote%%[!A-Za-z]*}
	remote_arch=$(printf '%s' "$platform_remote" | sed -n '2p' | tr -d ' ')
	case $remote_sys in
	Linux) ;;
	*) die "$destination is not Linux (uname said: $(printf '%s' "$platform_remote" | tr '\n' ' '))" ;;
	esac
fi
case $remote_arch in
x86_64 | amd64) remote_arch=amd64 ;;
aarch64 | arm64) remote_arch=arm64 ;;
*)
	die "$destination is not an amd64 or arm64 host (uname -m said: ${remote_arch:-nothing}); otter deploy builds those two architectures"
	;;
esac
say "host architecture is $remote_arch (login uid $login_uid)"
[ -n "$platform" ] || platform=linux/$remote_arch
case $platform in
*"/$remote_arch") ;;
*) die "--platform $platform does not match the host's $remote_arch" ;;
esac

# --- 2. the host is a fresh Ubuntu 24.04 ------------------------------------

say "checking the host is Ubuntu 24.04"
if [ "$dry_run" -eq 1 ]; then
	say "dry-run: would run: . /etc/os-release; printf '%s %s' \"\$ID\" \"\$VERSION_ID\""
else
	os=$(remote '. /etc/os-release 2>/dev/null && printf "%s %s" "$ID" "$VERSION_ID"') ||
		die "could not read /etc/os-release on $destination"
	os=$(printf '%s' "$os" | tr -d '\r')
	case $os in
	"ubuntu 24.04") ;;
	*) die "$destination is '$os', not 'ubuntu 24.04'; this script provisions Ubuntu 24.04 LTS" ;;
	esac
fi

# --- 3. packages and tools `otter deploy` needs -----------------------------
#
# The list is a superset of internal/deploy/remote.go's requiredTools: the same
# twelve, plus curl (deployer.go's post-deploy /health poll), python3 (the
# daemon needs an interpreter even with no Python jobs) and swapon (the
# fallback when /proc/swaps is unreadable). Superset on purpose: a host that
# passes here cannot then fail a check the deploy or the assertion later makes,
# and the two lists are allowed to drift only in this direction.

say "checking required tools are installed"
if [ "$dry_run" -eq 1 ]; then
	say "dry-run: would check for rsync systemctl install find chown ln getent groupadd useradd ss curl runuser sudo python3 swapon"
else
	tools="rsync systemctl install find chown ln getent groupadd useradd ss curl runuser sudo python3 swapon"
	tools_out=$(remote "missing=''
for tool in $tools; do
  command -v \"\$tool\" >/dev/null 2>&1 || missing=\"\$missing \$tool\"
done
printf '%s' \"\$missing\"") || die "could not check tools on $destination"
	tools_out=$(printf '%s' "$tools_out" | tr -d '\r')
	if [ -n "$tools_out" ]; then
		die "missing on $destination:$tools_out (Ubuntu: apt-get install -y rsync findutils sudo python3)"
	fi
	say "all required tools present"
fi

# --- 4. capacity: RAM, disk, swap -------------------------------------------
#
# Swap is asserted here and again on the host after the deploy. A host that
# silently lost its swapfile is exactly the failure this script exists to stop.

say "checking RAM, disk and swap"
if [ "$dry_run" -eq 1 ]; then
	say "dry-run: would assert MemTotal >= ${min_ram_mib} MiB, free space >= ${min_disk_mib} MiB, swap >= ${min_swap_mib} MiB"
else
	mem_kib=$(remote "awk '/^MemTotal:/ { print \$2 }' /proc/meminfo") ||
		die "could not read /proc/meminfo on $destination"
	mem_mib=$(mib "$mem_kib")
	[ "$mem_mib" -ge "$min_ram_mib" ] ||
		die "$destination has ${mem_mib} MiB RAM, below the ${min_ram_mib} MiB floor"
	say "RAM is ${mem_mib} MiB"

	disk_kib=$(remote "df -Pk $(quote "$remote_dir") 2>/dev/null | awk 'NR==2 { print \$4 }'") || true
	if [ -z "$disk_kib" ]; then
		disk_kib=$(remote "df -Pk / | awk 'NR==2 { print \$4 }'") ||
			die "could not check free disk space on $destination"
	fi
	disk_mib=$(mib "$disk_kib")
	[ "$disk_mib" -ge "$min_disk_mib" ] ||
		die "$destination has ${disk_mib} MiB free where the runtime will live, below the ${min_disk_mib} MiB floor"
	say "free space for $remote_dir is ${disk_mib} MiB"

	swap_kib=$(remote "awk 'NR > 1 { total += \$3 } END { print total + 0 }' /proc/swaps") ||
		die "could not read /proc/swaps on $destination"
	swap_mib=$(mib "$swap_kib")
	[ "$swap_mib" -ge "$min_swap_mib" ] ||
		die "$destination has ${swap_mib} MiB swap, below the ${min_swap_mib} MiB floor. On a 1 GiB host this fails under load, and the usual cause is a provisioning script that aborted before creating the swapfile: check 'cloud-init status --long' and /var/log/cloud-init-output.log, then create the swapfile before deploying."
	say "active swap is ${swap_mib} MiB"
fi

# --- 5. restricted SSH ------------------------------------------------------
#
# The host's only approved inbound port is 22 (CA-10), and that is only
# acceptable if sshd refuses passwords. Read the effective configuration, so a
# later drop-in cannot hide behind the file the image wrote. No reload is
# performed: socket-activated sshd re-reads its configuration, and provisioning
# must never abort on a reload.

say "checking sshd refuses passwords and root login"
if [ "$dry_run" -eq 1 ]; then
	say "dry-run: would run: sshd -T, then assert passwordauthentication=no and permitrootlogin=no|prohibit-password"
else
	sshd_out=$(remote 'sshd -T 2>/dev/null || sudo -n /usr/sbin/sshd -T 2>/dev/null') ||
		die "could not read the effective sshd configuration on $destination"
	sshd_out=$(printf '%s' "$sshd_out" | tr -d '\r')
	pw=$(printf '%s\n' "$sshd_out" | awk '$1 == "passwordauthentication" { print $2 }' | head -n 1)
	[ "$pw" = "no" ] || die "sshd on $destination has passwordauthentication=${pw:-unset}, want no"
	root_login=$(printf '%s\n' "$sshd_out" | awk '$1 == "permitrootlogin" { print $2 }' | head -n 1)
	case $root_login in
	no | prohibit-password) ;;
	*) die "sshd on $destination has permitrootlogin=${root_login:-unset}, want no or prohibit-password" ;;
	esac
	say "sshd: passwordauthentication=no permitrootlogin=$root_login"
fi

# --- 6. the host is fresh ---------------------------------------------------

say "checking the host is not already provisioned"
if [ "$dry_run" -eq 1 ]; then
	say "dry-run: would check for the $service_user account, $remote_dir/workspaces and /etc/otter/workspaces"
else
	# Each probe is independent: `A && mark; B && mark` stops at the first
	# false, so a host with the account but no workspace was reported as
	# "already provisioned" using only its account and the other two halves of
	# the check were never exercised.
	existing=""
	if remote "id -u $(quote "$service_user") >/dev/null 2>&1"; then
		existing="$existing service-account:$service_user"
	fi
	if remote "test -d $(quote "$remote_dir/workspaces")"; then
		existing="$existing $remote_dir/workspaces"
	fi
	if remote "test -e /etc/otter/workspaces"; then
		existing="$existing /etc/otter/workspaces"
	fi
	if [ -n "$existing" ]; then
		if [ "$allow_existing" -eq 1 ]; then
			say "host is already provisioned (${existing# }); converging because --allow-existing"
		else
			die "$destination is already provisioned (${existing# }). Re-run with --allow-existing to converge it, or confirm this is the intended host."
		fi
	else
		say "host is fresh"
	fi
fi

# --- 7. the report the provisioner was supposed to leave --------------------
#
# Present only when the image's own provisioning completed. A missing report is
# the fingerprint of the failed cloud-init user-data described in the header. It
# is checked before the deploy so the operator learns about it while the cause
# is fresh, and checked again by the permission assertion afterwards.

say "checking the provisioning report exists"
if [ "$dry_run" -eq 1 ]; then
	say "dry-run: would require a non-empty /var/log/otter-provision-report.txt"
else
	report=$(remote 'test -s /var/log/otter-provision-report.txt && cat /var/log/otter-provision-report.txt' 2>/dev/null) || true
	report=$(printf '%s' "$report" | tr -d '\r')
	if [ -n "$report" ]; then
		say "provisioning report present: $(printf '%s' "$report" | head -n 1)"
	else
		die "$destination has no /var/log/otter-provision-report.txt. The image's provisioning did not complete; run 'cloud-init status --long' and read /var/log/cloud-init-output.log on the host. If this host was provisioned some other way, deploy it and then run scripts/assert-host-permissions.sh on it with --provision-report '' to record that decision."
	fi
fi

# --- 8. deploy --------------------------------------------------------------
#
# Everything above was read-only. This is the first step that changes the host,
# and it is `otter deploy` -- not a reimplementation of it.

deploy_cmd=$otter_bin" deploy"
deploy_cmd=$deploy_cmd" --host $(quote "$host")"
deploy_cmd=$deploy_cmd" --user $(quote "$ssh_user")"
deploy_cmd=$deploy_cmd" --port $ssh_port"
[ -z "$identity" ] || deploy_cmd=$deploy_cmd" --identity $(quote "$identity")"
deploy_cmd=$deploy_cmd" --remote-dir $(quote "$remote_dir")"
deploy_cmd=$deploy_cmd" --service-user $(quote "$service_user")"
deploy_cmd=$deploy_cmd" --platform $(quote "$platform")"
deploy_cmd=$deploy_cmd" --daemon-env $(quote "$daemon_env_deploy")"

say "deploying"
if [ "$dry_run" -eq 1 ]; then
	printf 'provision: would run: %s\n' "$deploy_cmd"
else
	(cd "$project" && sh -c "$deploy_cmd") || die "otter deploy failed on $destination"
	say "otter deploy succeeded"
fi

# --- 9. assert the host the deploy produced ---------------------------------

say "asserting permissions, ingress and swap on the host"
remote_assert=/tmp/otter-assert-host-permissions.sh
if [ "$dry_run" -eq 1 ]; then
	printf 'provision: would upload %s to %s\n' "$assert_script" "$remote_assert"
	printf 'provision: would run on host: sudo -n sh %s --user %s --workspace-dir %s/workspaces/<workspace> --approved-ports 22\n' \
		"$remote_assert" "$service_user" "$remote_dir"
else
	# The script is uploaded over stdin and run with sudo: it reads /etc/otter,
	# the unit and /proc/swaps, none of which a non-root login can always read.
	# shellcheck disable=SC2086
	ssh $ssh_args "$destination" "sudo -n tee $(quote "$remote_assert") >/dev/null" <"$assert_script" ||
		die "could not upload $assert_script to $destination"
	# The workspace directory is discovered on the host rather than guessed: its
	# name carries a workspace id this script never sees.
	workspace_dir=$(remote "ls -d $(quote "$remote_dir/workspaces")/*/ 2>/dev/null | head -n 1 | sed 's:/$::'") ||
		die "could not list workspaces under $remote_dir on $destination"
	workspace_dir=$(printf '%s' "$workspace_dir" | tr -d '\r')
	[ -n "$workspace_dir" ] ||
		die "no workspace under $remote_dir on $destination; the deploy reported success but installed nothing"
	say "asserting against $workspace_dir"
	# shellcheck disable=SC2086
	ssh $ssh_args "$destination" \
		"sudo -n sh $(quote "$remote_assert") --user $(quote "$service_user") --workspace-dir $(quote "$workspace_dir") --approved-ports 22" ||
		die "the host does not meet CA-09/CA-10; the deploy ran but the host state is not safe"
	say "host assertions passed"
fi

if [ "$dry_run" -eq 1 ]; then
	# A dry run must not claim an outcome it did not produce.
	printf 'provision: dry run complete — no change was made to %s\n' "$destination"
else
	printf 'provision: OK — %s is provisioned and asserted\n' "$destination"
fi
