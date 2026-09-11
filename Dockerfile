# Otter runtime image.
#
# IMPORTANT: the binaries are NOT built inside this image. Run
#     make build
# (or `make cross` and pick the matching linux binary) before `docker build`,
# then build the image from the repository root:
#     make docker
#     # or: docker build -t otter:dev .
#
# Integrations are plain Python programs, so the image only needs a Python
# interpreter plus the two static binaries. The Python SDK travels inside the
# otterd binary and is extracted to <data dir>/sdk/python on first start, so
# there is nothing to pip install.

FROM python:3.13-slim

LABEL org.opencontainers.image.title="Otter" \
      org.opencontainers.image.description="Lightweight self-hosted runtime for Python integrations" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/otter-run/otter" \
      org.opencontainers.image.documentation="https://github.com/otter-run/otter/blob/main/docs/operations.md"

# NOTE: integrations execute as the container user, with that user's full
# permissions (Otter does not sandbox them in the MVP). Anyone who can add a
# directory with an otter.yaml to the integrations root can run code in this
# container. Prefer one container per trust boundary, and use a dedicated,
# unprivileged user or a separate container/VM for anything untrusted.

COPY bin/otterd /usr/local/bin/otterd
COPY bin/otter  /usr/local/bin/otter

# SQLite database, extracted SDK and default integration root live here.
# Mount a volume (or bind-mount a host directory) to persist state and history.
WORKDIR /var/lib/otter
VOLUME ["/var/lib/otter"]

EXPOSE 7337

# The health endpoint requires no token on the loopback interface, and the
# Python interpreter is guaranteed to be present in this image.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD python3 -c "import urllib.request;urllib.request.urlopen('http://127.0.0.1:7337/health').read()" || exit 1

# Override the defaults at run time, e.g.
#   docker run --rm -p 7337:7337 \
#     -v "$PWD/examples:/var/lib/otter/integrations:ro" \
#     -v otter-data:/var/lib/otter \
#     otter:dev --integrations /var/lib/otter/integrations --listen 0.0.0.0:7337 \
#     --api-token "$OTTER_API_TOKEN"
# Binding to a non-loopback address requires --api-token / OTTER_API_TOKEN.
ENTRYPOINT ["otterd"]
