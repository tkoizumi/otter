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
# The interpreter managed jobs pin in .python-version. It is provisioned at build
# time because a preparation sandbox has no egress: uv would otherwise have to
# download it at first use, which cannot happen inside the sandbox and turned
# class C preparation into a network failure rather than a measurement.
ARG PYTHON_VERSION=3.13.5
# Recorded in the image so a running container can be tied back to the source it
# was built from, which is what makes a qualification result attributable.
LABEL org.otter.version="${OTTER_VERSION}" \
      org.otter.component="tenant-runtime" \
      org.otter.plan="POOLED-RUNTIME-IMPLEMENTATION-PLAN WP1"

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    UV_CACHE_DIR=/tmp/uv/cache \
    UV_PYTHON_INSTALL_DIR=/opt/otter/python \
    UV_TOOL_DIR=/tmp/uv/tools \
    UV_TOOL_BIN_DIR=/tmp/uv/bin

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
      util-linux \
 && rm -rf /var/lib/apt/lists/*

# uv prepares managed Python environments. Pinned, and installed for the
# unprivileged user only: preparation runs as the tenant, so it must not have a
# writable toolchain owned by root. The interpreter it installs below is global
# and read-only, which is the distinction that matters.
#
# The pin is not arbitrary and was not 0.4.30 for long: that version reports
# "No download found for request: cpython-3.13.5-linux-aarch64-gnu" on arm64, so
# it cannot provision the interpreter the fixtures pin. A uv pin that cannot
# install the pinned interpreter is a broken image, not a reproducible one.
COPY --from=ghcr.io/astral-sh/uv:0.7.13 /uv /usr/local/bin/uv

# The runtime binary. Copied, not downloaded: see the build arguments above.
COPY bin/otterd /usr/local/bin/otterd
COPY bin/otter /usr/local/bin/otter

# The supervisor that starts both processes in this one container: otterd as uid
# 10001 and the agent as uid 10002, over loopback. See the file for why they are
# in one container and how the volume isolation is enforced.
COPY packaging/container/entrypoint.sh /usr/local/bin/otter-entrypoint
RUN chmod 0755 /usr/local/bin/otter-entrypoint

# The interpreter is provisioned into the image at BUILD time, under
# UV_PYTHON_INSTALL_DIR, which is where uv looks for it at run time. That
# directory is read-only at run time -- correctly, since an interpreter is not
# tenant state -- and is why the variable points there rather than at /tmp.
#
# Provisioning it here is what makes an offline preparation possible at all: a
# preparation sandbox has no egress, so an interpreter uv had to download at
# first use turned class C preparation into a network failure instead of a
# measurement.
RUN UV_PYTHON_INSTALL_DIR=/opt/otter/python \
      /usr/local/bin/uv python install "${PYTHON_VERSION}" \
 && chmod -R a+rX /opt/otter/python

# TWO dedicated unprivileged identities. The tenant volume is 0700 and owned by
# uid 10001, so the agent's uid 10002 cannot read or write it: that is the
# isolation invariant between the agent and tenant data. The uids are fixed
# rather than allocated so a volume mounted from the host can be ownership-checked
# against known values.
#
# The container STARTS AS ROOT only to drop to these two uids; entrypoint.sh does
# the drop, and the provisioner grants exactly SETUID, SETGID and CHOWN for it.
# No process holds a capability after that.
RUN groupadd --gid 10001 otter \
 && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin otter \
 && groupadd --gid 10002 otter-agent \
 && useradd --uid 10002 --gid 10002 --no-create-home --shell /usr/sbin/nologin otter-agent \
 && install -d -o otter -g otter -m 0700 /workspace \
 && install -d -o otter -g otter -m 0700 /tmp/otter \
 && install -d -o otter -g otter -m 0755 /tmp/uv

# Everything the runtime writes lives under /workspace (the tenant volume) or
# /tmp (bounded temporary storage). The root filesystem stays read-only at run
# time, so nothing else may be a write target.
#
# The UV_* variables above are the reason this works at all. uv defaults to
# $HOME/.local/share/uv for the interpreters it installs, and $HOME is on the
# read-only root, so a managed job failed with "failed to create directory
# /home/otter/.local/share/uv/python: Read-only file system" -- the image could
# not prepare a managed environment, which is the class C workload. Pointing
# every uv write target at /tmp keeps the root read-only and the tenant's
# temporary storage bounded and explicit.
#
# /tmp/uv is created here so the directory exists with the right ownership; the
# run itself mounts a bounded tmpfs over /tmp.
VOLUME ["/workspace"]
WORKDIR /workspace

# Documentation only. The daemon binds loopback inside the sandbox and the
# controlled ingress route is what exposes it; publishing a port here would
# suggest otherwise.
EXPOSE 7337

# Both processes live in this one container so they share a network namespace and
# speak over loopback: gVisor cannot share a namespace between containers, which
# is why the runtime is not a separate sandboxed sidecar. entrypoint.sh drops to
# uid 10001 for otterd and 10002 for the agent.
#
# /workspace holds the database, releases and environments; it is the volume and
# nothing else is. --data and --jobs both point at it deliberately: the pool gives
# a tenant one tree, and a second writable root would be a place for state to
# escape the volume that is backed up and moved. The agent's own data goes to a
# tmpfs the tenant cannot read.
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/otter-entrypoint"]
