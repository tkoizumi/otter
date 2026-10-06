#!/usr/bin/env sh
# Read back the limits a tenant container is ACTUALLY running under.
#
# The launch flags are a request. This reads the kernel's view -- cgroup files
# and process state -- so a qualification result records what was enforced
# rather than what was asked for. A sandbox flag that silently did not apply
# looks exactly like a passing test otherwise.
#
# Usage: verify-limits.sh <container> [expected-memory-bytes] [expected-pids]
set -eu

name=${1:?usage: verify-limits.sh <container> [memory-bytes] [pids]}
want_mem=${2:-}
want_pids=${3:-}

docker inspect "$name" >/dev/null 2>&1 || { echo "no such container: $name" >&2; exit 2; }

cid=$(docker inspect -f '{{.Id}}' "$name")
cgroup="/sys/fs/cgroup/system.slice/docker-${cid}.scope"
[ -d "$cgroup" ] || cgroup="/sys/fs/cgroup/docker/${cid}"
[ -d "$cgroup" ] || { echo "FAIL: cannot locate the cgroup for $name" >&2; exit 1; }

fail=0
check() {
  # check <label> <actual> <expected> ; empty expected means report only
  label=$1; actual=$2; expected=$3
  if [ -z "$expected" ]; then
    printf '  %-22s %s\n' "$label" "$actual"
  elif [ "$actual" = "$expected" ]; then
    printf '  %-22s %s (as requested)\n' "$label" "$actual"
  else
    printf '  %-22s %s  WANTED %s\n' "$label" "$actual" "$expected"
    fail=1
  fi
}

echo "container: $name"
echo "effective limits:"
check "memory.max"      "$(cat "$cgroup/memory.max" 2>/dev/null || echo n/a)" "$want_mem"
check "memory.swap.max" "$(cat "$cgroup/memory.swap.max" 2>/dev/null || echo n/a)" "$want_mem"
check "pids.max"        "$(cat "$cgroup/pids.max" 2>/dev/null || echo n/a)" "$want_pids"
check "cpu.max"         "$(cat "$cgroup/cpu.max" 2>/dev/null || echo n/a)" ""

ro=$(docker inspect -f '{{.HostConfig.ReadonlyRootfs}}' "$name")
caps=$(docker inspect -f '{{.HostConfig.CapDrop}}' "$name")
nnp=$(docker inspect -f '{{.HostConfig.SecurityOpt}}' "$name")
user=$(docker inspect -f '{{.Config.User}}' "$name")
runt=$(docker inspect -f '{{.HostConfig.Runtime}}' "$name")
echo "effective isolation:"
check "readonly_rootfs" "$ro" "true"
check "cap_drop"        "$caps" "[ALL]"
check "user"            "$user" "10001:10001"
printf '  %-22s %s\n' "security_opt" "$nnp"
printf '  %-22s %s\n' "runtime" "$runt"

# A read-only root that is not actually read-only is the failure this exists to
# catch, so attempt a write rather than trusting the flag.
if docker exec "$name" sh -c 'touch /etc/otter-write-probe' 2>/dev/null; then
  echo "FAIL: the root filesystem accepted a write"
  docker exec "$name" rm -f /etc/otter-write-probe 2>/dev/null || true
  fail=1
else
  echo "  root filesystem rejected a write (as intended)"
fi

[ "$fail" -eq 0 ] && echo "RESULT: all checked limits match" || echo "RESULT: MISMATCH - see above"
exit "$fail"
