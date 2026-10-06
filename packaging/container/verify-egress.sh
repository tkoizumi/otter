#!/bin/sh
# Verify a tenant container's egress policy by CONNECTING, not by resolving.
#
# DNS-level filtering is not an egress policy: a name whose resolution is
# "blocked" can still be reached by connecting to the address directly, and a
# name can be made to resolve to a forbidden address after the check passed
# (DNS rebinding). So every probe here opens a socket, and the pass condition is
# that the connection fails.
#
# The probes are separate files copied into the container. An earlier version
# embedded them in a heredoc piped through `docker exec`, which never reached the
# process -- every probe was silent, and silence read exactly like a clean pass.
#
# Usage: verify-egress.sh <container> [profile]
set -eu

name=${1:?usage: verify-egress.sh <container> [profile]}
profile=${2:-shared-public}
here=$(cd "$(dirname "$0")" && pwd)

docker inspect "$name" >/dev/null 2>&1 || { echo "no such container: $name" >&2; exit 2; }

# python3 rather than curl: the image carries python3 and does not carry curl, and
# a probe that fails because its tool is missing reports a boundary never tested.
# Installed through the shell, not `docker cp`. The launch contract makes the
# root filesystem read-only and gives the container a writable /tmp tmpfs;
# `docker cp` writes via the daemon into the container's rootfs path and fails
# against the read-only mount even though /tmp is writable inside.
for probe in egress_probe.py rebind_probe.py; do
  b64=$(base64 -w0 < "$here/$probe" 2>/dev/null || base64 < "$here/$probe" | tr -d '\n')
  printf '%s' "$b64" | docker exec -i "$name" sh -c "base64 -d > /tmp/$probe" 2>/dev/null \
    || { echo "cannot install $probe into $name" >&2; exit 1; }
  docker exec "$name" test -s "/tmp/$probe" \
    || { echo "$probe is empty after install; the probe would report nothing" >&2; exit 1; }
done

echo "container: $name"
echo "egress_profile: $profile"
echo
echo "== must be BLOCKED (a REACHABLE line is a failure of the boundary) =="
docker exec "$name" python3 /tmp/egress_probe.py \
  "ipv4-metadata|169.254.169.254|80|AF_INET" \
  "ipv4-link-local|169.254.1.1|80|AF_INET" \
  "ipv4-loopback|127.0.0.1|22|AF_INET" \
  "ipv4-rfc1918-10|10.0.0.1|22|AF_INET" \
  "ipv4-rfc1918-172|172.16.0.1|22|AF_INET" \
  "ipv4-rfc1918-192|192.168.0.1|22|AF_INET" \
  "ipv6-loopback|::1|22|AF_INET6" \
  "ipv6-link-local|fe80::1|22|AF_INET6" \
  "ipv6-ula|fd00::1|22|AF_INET6" \
  "ipv6-mapped-metadata|::ffff:169.254.169.254|80|AF_INET6" \
  || echo "  (probe errored)"

echo
echo "== DNS rebinding: a NAME that resolves to a blocked address =="
docker exec "$name" python3 /tmp/rebind_probe.py || echo "  (probe errored)"

echo
echo "== profile check =="
net=$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$name")
printf '  NetworkMode = %s\n' "$net"
case "$net" in
  bridge|default)
    echo "  FAIL: Docker's default bridge, where this qualification MEASURED the" ;;
  *)
    echo "  not the default bridge - acceptable" ;;
esac
case "$net" in
  bridge|default)
    echo "        instance metadata service and the host's SSH port reachable" ;;
esac
echo
echo "A REACHABLE line above is a failure of the boundary, not of this script."
