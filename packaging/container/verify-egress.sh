#!/bin/sh
# Verify a tenant container's egress policy by CONNECTING, not by resolving.
#
# DNS-level filtering is not an egress policy: a name whose resolution is
# "blocked" can still be reached by connecting to the address directly, and a
# name can be made to resolve to a forbidden address after the check passed
# (DNS rebinding). So every probe here opens a socket, and the pass condition is
# that the connection fails.
#
# Usage: verify-egress.sh <container> [profile]
set -eu

name=${1:?usage: verify-egress.sh <container> [profile]}
profile=${2:-shared-public}

docker inspect "$name" >/dev/null 2>&1 || { echo "no such container: $name" >&2; exit 2; }

# The probe runs inside the container, because that is where the policy applies.
# python3 is used rather than curl: the image carries python3 and does not carry
# curl, and a probe that fails because its tool is missing reports a boundary
# that was never tested.
probe() {
  docker exec "$name" python3 - "$@" <<'PY'
import socket, sys, time

def connect(host, port, label, family=socket.AF_UNSPEC):
    s = socket.socket(family, socket.SOCK_STREAM)
    s.settimeout(4)
    t0 = time.monotonic()
    try:
        s.connect((host, port))
        print(f"REACHABLE {label} ({host}:{port})")
    except Exception as e:
        print(f"blocked   {label} ({host}:{port}) - {type(e).__name__}")
    finally:
        s.close()

for spec in sys.argv[1:]:
    label, host, port, fam = spec.split("|")
    connect(host, int(port), label, getattr(socket, fam))
PY
}

echo "container: $name"
echo "egress_profile: $profile"
echo
echo "== must be BLOCKED (a connection that succeeds is a failure) =="
probe \
  "ipv4-metadata|169.254.169.254|80|AF_INET" \
  "ipv4-link-local|169.254.1.1|80|AF_INET" \
  "ipv4-loopback|127.0.0.1|22|AF_INET" \
  "ipv4-rfc1918-10|10.0.0.1|22|AF_INET" \
  "ipv4-rfc1918-172|172.16.0.1|22|AF_INET" \
  "ipv4-rfc1918-192|192.168.0.1|22|AF_INET" \
  "ipv6-loopback|::1|22|AF_INET6" \
  "ipv6-link-local|fe80::1|22|AF_INET6" \
  "ipv6-mapped-metadata|::ffff:169.254.169.254|80|AF_INET6"

echo
echo "== DNS rebinding: a NAME that resolves to a blocked address =="
echo "   Resolution succeeding is fine. Connecting to the result must fail."
docker exec "$name" python3 - <<'PY'
import socket
# nip.io echoes back whatever address is encoded in the name, so this resolves
# to the metadata service without any DNS server being under our control --
# exactly the rebinding case a name-based block would miss.
for host in ("169.254.169.254.nip.io", "127.0.0.1.nip.io"):
    try:
        addr = socket.gethostbyname(host)
        print(f"  resolved  {host} -> {addr}  (resolution allowed)")
    except Exception as e:
        print(f"  resolved  {host} -> FAILED ({type(e).__name__}) - no DNS egress; also acceptable")
        continue
    s = socket.socket(); s.settimeout(4)
    try:
        s.connect((addr, 80)); print(f"  REACHABLE {host} -> {addr}  <-- REBINDING BYPASSED THE POLICY")
    except Exception as e:
        print(f"  blocked   {host} -> {addr}  ({type(e).__name__})")
    finally:
        s.close()
PY

echo
echo "== profile check =="
net=$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$name")
printf '  NetworkMode = %s\n' "$net"
case "$net" in
  none|*net*|*internal*)
    echo "  not the default bridge - acceptable" ;;
  bridge|default)
    echo "  FAIL: the tenant is on Docker's default bridge, where metadata and host services are reachable" ;;
esac
echo
echo "A REACHABLE line above is a failure of the boundary, not of this script."
