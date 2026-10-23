#!/bin/sh
# Falsifiability for packaging/container/healthcheck.sh, the image HEALTHCHECK.
#
# The healthcheck answers one question: is a LIVE process running as the agent's
# uid? It exists because the container's own state only tracks otterd, while the
# agent is a second process in the same container. A check that counted a zombie
# would stay green in the failure it exists to catch, so the State rule is the
# part worth pinning down.
#
# The suite runs the real script against two inputs:
#
#   * the real /proc, for the positive case (the suite's own uid is live while a
#     `sleep 30 &` probe runs) and for a uid that owns nothing (54321);
#   * a fixture procfs through OTTER_PROC_ROOT, for the classifications that
#     cannot be created deterministically for an arbitrary uid without root:
#     R/S/D are alive, Z and T are not, a zombie beside a live process is still
#     alive, and an empty procfs is "no process". The fixture is the script's
#     real input format (Uid:/State: lines), not a stub of the script.
#
# A real zombie CAN be made on Linux (a backgrounded grandchild whose parent
# execs a long sleep), but it carries the suite's own uid and the suite's shell
# shares that uid, so it can never be the only process with it; isolating one
# would need root or a uid of its own. The zombie case therefore presents a
# status file in the kernel's exact format -- a real zombie reads
# `Uid:\t<uid>...` and `State:\tZ (zombie)` -- and runs the real script against
# it. The static [RSD] check below pins the class as well, so the case does not
# rest on the fixture alone, and the falsifiability case proves it can go red.
#
# WHY THE LITERAL "kill the sleep, then the same uid must fail" IS NOT ASSERTED
# ON THE REAL /proc: `make test-shell` runs this suite as the invoking user, so
# the suite's own shell keeps a live process with that uid after the probe is
# killed and the healthcheck correctly still succeeds. Making that assertion
# true would need root to drop the probe to a dedicated uid (or a PID
# namespace), neither of which `make test-shell` has. The same transition --
# live process present, then absent -- is exercised against the fixture procfs,
# which goes through the identical code path.
#
# The last case is falsifiability: widening the State class from [RSD] to
# [RSDZ] in a copy of the script must make the zombie case red, proving the
# zombie assertion is reading the State rule rather than assuming.
#
#   sh scripts/test-container-health.sh
#
# OTTER_HEALTHCHECK_SUBJECT / OTTER_DOCKERFILE_SUBJECT point the suite at
# mutated copies, the same way the other suites in this directory do.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/.." && pwd)
healthcheck=${OTTER_HEALTHCHECK_SUBJECT:-$root/packaging/container/healthcheck.sh}
dockerfile=${OTTER_DOCKERFILE_SUBJECT:-$root/packaging/container/Dockerfile}
subject=$healthcheck
me=$(id -u)

[ -f "$healthcheck" ] || {
	echo "test-container-health: missing $healthcheck" >&2
	exit 2
}
[ -f "$dockerfile" ] || {
	echo "test-container-health: missing $dockerfile" >&2
	exit 2
}
[ -r /proc/self/status ] || {
	echo "test-container-health: this suite needs a mounted /proc" >&2
	exit 2
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/otter-healthcheck.XXXXXX")
sleep_pid=
cleanup() {
	if [ -n "$sleep_pid" ]; then
		kill "$sleep_pid" 2>/dev/null || true
		wait "$sleep_pid" 2>/dev/null || true
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT HUP INT TERM

passed=0
failed=0
pass() { passed=$((passed + 1)); echo "ok   - $1"; }
fail() { failed=$((failed + 1)); echo "FAIL - $1" >&2; }

# run_health runs the subject with a uid and, when given, a fixture proc root.
# Callers use it in an `if` so an expected non-zero exit does not trip set -e.
run_health() { # uid [proc_root]
	h_uid=$1
	h_proc=${2-}
	if [ -n "$h_proc" ]; then
		OTTER_AGENT_UID=$h_uid OTTER_PROC_ROOT=$h_proc sh "$subject" 2>"$tmp/stderr"
	else
		OTTER_AGENT_UID=$h_uid sh "$subject" 2>"$tmp/stderr"
	fi
}

check_exit() { # label want uid [proc_root]
	c_label=$1
	c_want=$2
	c_uid=$3
	c_proc=${4-}
	if run_health "$c_uid" "$c_proc"; then
		c_got=0
	else
		c_got=$?
	fi
	if [ "$c_got" -eq "$c_want" ]; then
		pass "$c_label"
	else
		fail "$c_label (exit $c_got, want $c_want; stderr: $(cat "$tmp/stderr"))"
	fi
}

# mkstatus writes the two lines the healthcheck reads, in the kernel's format:
# four uid values and a letter-led State line.
mkstatus() { # path state uid
	mkdir -p "$(dirname "$1")"
	{
		printf 'Name:\totter-agent\n'
		printf 'Uid:\t%s\t%s\t%s\t%s\n' "$3" "$3" "$3" "$3"
		printf 'Gid:\t%s\t%s\t%s\t%s\n' "$3" "$3" "$3" "$3"
		printf 'State:\t%s\n' "$2"
	} >"$1"
}

# --- real /proc ---------------------------------------------------------------
# The probe process is real; the suite's own shell also holds uid $me, so this
# proves the positive path, not that the probe alone kept it alive.
sleep 30 &
sleep_pid=$!
if run_health "$me"; then
	pass "a live process with uid $me is alive on the real /proc"
else
	fail "the real /proc was not seen as alive for uid $me: $(cat "$tmp/stderr")"
fi
kill "$sleep_pid" 2>/dev/null || true
wait "$sleep_pid" 2>/dev/null || true
sleep_pid=

if [ "$me" -eq 54321 ]; then
	pass "the real uid 54321 is the suite's own; the no-process case uses the fixture below"
else
	check_exit "a uid that owns no process on the real /proc is not alive" 1 54321
fi

# --- fixture procfs: live states ---------------------------------------------
for state in "R (running)" "S (sleeping)" "D (disk sleep)"; do
	proc=$tmp/proc-live-${state%% *}
	mkstatus "$proc/1001/status" "$state" "$me"
	check_exit "State ${state%% *} counts as alive" 0 "$me" "$proc"
done

# --- fixture procfs: a zombie is not alive -----------------------------------
zproc=$tmp/proc-zombie
mkstatus "$zproc/4242/status" "Z (zombie)" "$me"
if run_health "$me" "$zproc"; then
	z_got=0
else
	z_got=$?
fi
if [ "$z_got" -eq 1 ] && grep -q "$me" "$tmp/stderr" && grep -qi zombie "$tmp/stderr"; then
	pass "a zombie with the agent uid is not alive, and stderr names the uid and the zombie"
else
	fail "a zombie was not rejected with a uid-naming diagnostic (exit $z_got: $(cat "$tmp/stderr"))"
fi

tproc=$tmp/proc-stopped
mkstatus "$tproc/4243/status" "T (stopped)" "$me"
check_exit "a stopped (State T) process is not alive" 1 "$me" "$tproc"

# --- fixture procfs: no process, and mixed sets ------------------------------
oproc=$tmp/proc-other-uid
mkstatus "$oproc/5000/status" "S (sleeping)" 54321
check_exit "a fixture uid that owns nothing is not alive" 1 "$me" "$oproc"

empty=$tmp/proc-empty
mkdir -p "$empty"
check_exit "an empty proc root is not alive" 1 "$me" "$empty"

mproc=$tmp/proc-mixed-live
mkstatus "$mproc/6001/status" "Z (zombie)" "$me"
mkstatus "$mproc/6002/status" "S (sleeping)" "$me"
check_exit "a zombie beside a live process is still alive" 0 "$me" "$mproc"

dproc=$tmp/proc-mixed-dead
mkstatus "$dproc/7001/status" "Z (zombie)" "$me"
mkstatus "$dproc/7002/status" "T (stopped)" "$me"
check_exit "zombies and stopped processes only is not alive" 1 "$me" "$dproc"

# --- the script states the rule the check relies on --------------------------
if grep -Fq 'grep -q "^Uid:[[:space:]]*$uid"' "$healthcheck"; then
	pass "the healthcheck matches the agent uid in /proc/<pid>/status"
else
	fail "the healthcheck no longer matches Uid: for the agent uid"
fi
if grep -Fq 'grep -q "^State:[[:space:]]*[RSD]"' "$healthcheck"; then
	pass "the healthcheck's live-state class is exactly R, S and D"
else
	fail "the healthcheck's State: class is not [RSD] (a zombie may have crept in)"
fi

# --- the image actually installs and runs it ---------------------------------
if grep -q '^HEALTHCHECK ' "$dockerfile"; then
	pass "the Dockerfile declares a HEALTHCHECK"
else
	fail "the Dockerfile declares no HEALTHCHECK"
fi
if grep -Fq 'COPY packaging/container/healthcheck.sh /usr/local/bin/otter-healthcheck' "$dockerfile"; then
	pass "the Dockerfile installs the healthcheck at /usr/local/bin/otter-healthcheck"
else
	fail "the Dockerfile does not COPY the healthcheck to /usr/local/bin/otter-healthcheck"
fi
if grep -Eq 'chmod 0?755 /usr/local/bin/otter-healthcheck' "$dockerfile"; then
	pass "the Dockerfile makes the healthcheck executable"
else
	fail "the Dockerfile does not chmod 0755 /usr/local/bin/otter-healthcheck"
fi
if grep -Fq 'CMD ["/usr/local/bin/otter-healthcheck"]' "$dockerfile"; then
	pass "the HEALTHCHECK runs /usr/local/bin/otter-healthcheck"
else
	fail "the HEALTHCHECK does not run /usr/local/bin/otter-healthcheck"
fi
for flag in --interval=30s --timeout=5s --start-period=60s --retries=3; do
	if grep -Fq -e "$flag" "$dockerfile"; then
		pass "the HEALTHCHECK sets $flag"
	else
		fail "the HEALTHCHECK does not set $flag"
	fi
done

# --- falsifiability: the zombie rule can go red ------------------------------
sed 's/\[RSD\]/[RSDZ]/' "$healthcheck" >"$tmp/mutated.sh"
if grep -Fq '[RSDZ]' "$tmp/mutated.sh"; then
	subject=$tmp/mutated.sh
	if run_health "$me" "$zproc"; then
		pass "the zombie rule is falsifiable (accepting State Z makes the check green)"
	else
		fail "the mutated check still rejected a zombie; the case reads something else"
	fi
	subject=$healthcheck
else
	fail "could not mutate the State class in a copy of the healthcheck"
fi

# --- outcome -----------------------------------------------------------------
echo ""
if [ "$failed" -gt 0 ]; then
	echo "test-container-health: FAILED ($failed check(s))" >&2
	exit 1
fi
echo "test-container-health: $passed passed, 0 failed"
