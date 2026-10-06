# Production image for one pooled tenant runtime.
#
# This is not a drill image. The drills under scripts/drill/ run privileged with
# host cgroups and a writable root because they are testing systemd behaviour;
# this container is the thing a tenant's jobs actually run inside, so every
# choice below is a boundary rather than a convenience.
#
# Build arguments pin both inputs, because "latest" is not a reproducible
# qualification and the plan requires the base and the binary identified by
# version:
#
#   docker build \
#     --build-arg OTTER_VERSION=v0.4.0-11-gc027d9c \
#     -f packaging/container/Dockerfile \
#     -t otter-runtime:c027d9c .
#
# The context is the REPOSITORY ROOT, not this directory: the COPY instructions
# below name bin/otterd and bin/otter, which live at the root. Building from this
# directory instead makes the context ~800 bytes and fails with "checksum ...
# /bin/otter: not found" for a file that exists, because the path is resolved
# relative to the context.
#
# The binary is copied in rather than fetched so the image cannot silently
# change when a URL does; the build context carries `bin/otterd` and
# `bin/otter` produced by `make build`.
FROM debian:bookworm-slim AS runtime

ARG OTTER_VERSION=unspecified
ARG PYTHON_VERSION=3.13
# Recorded in the image so a running container can be tied back to the source it
# was built from, which is what makes a qualification result attributable.
LABEL org.otter.version="${OTTER_VERSION}" \
      org.otter.component="tenant-runtime" \
      org.otter.plan="POOLED-RUNTIME-IMPLEMENTATION-PLAN WP1"

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

# ca-certificates is not optional: every integration class in the qualification
# contract talks to a vendor HTTPS API, and a runtime without a trust store
# fails in a way that looks like a network block.
#
# tini is PID 1. otterd reaps its own children, but it does not reap everything
# in the container, so an orphan from a cancelled run would otherwise accumulate
# against the process limit -- the count a tenant is admitted under. tini also
# forwards signals, so SIGTERM reaches otterd rather than stopping at PID 1.
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      ca-certificates \
      libsqlite3-0 \
      python3 \
      python3-venv \
      tini \
      tzdata \
 && rm -rf /var/lib/apt/lists/*

# uv prepares managed Python environments. Pinned, and installed for the
# unprivileged user only: preparation runs as the tenant, so it must not have a
# writable toolchain owned by root.
COPY --from=ghcr.io/astral-sh/uv:0.4.30 /uv /usr/local/bin/uv

# The runtime binary. Copied, not downloaded: see the build arguments above.
COPY bin/otterd /usr/local/bin/otterd
COPY bin/otter /usr/local/bin/otter

# A dedicated unprivileged identity. The uid is fixed rather than allocated so
# that a tenant volume mounted from the host can be ownership-checked against a
# known value instead of whatever the image happened to pick.
RUN groupadd --gid 10001 otter \
 && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin otter \
 && install -d -o otter -g otter -m 0755 /workspace \
 && install -d -o otter -g otter -m 0700 /tmp/otter

# Everything the runtime writes lives under /workspace (the tenant volume) or
# /tmp (bounded temporary storage). The root filesystem stays read-only at run
# time, so nothing else may be a write target.
VOLUME ["/workspace"]
WORKDIR /workspace
USER 10001:10001

# Documentation only. The daemon binds loopback inside the sandbox and the
# controlled ingress route is what exposes it; publishing a port here would
# suggest otherwise.
EXPOSE 7337

# The runtime is a stateful singleton. /workspace holds the database, releases
# and environments, so it is the volume and nothing else is mounted.
#
# --data and --jobs both point at /workspace deliberately: the pool gives a
# tenant one tree, and a second writable root would be a place for state to
# escape the volume that is backed up and moved.
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/otterd", \
     "--jobs", "/workspace", \
     "--data", "/workspace/.otter/data", \
     "--listen", "127.0.0.1:7337"]
