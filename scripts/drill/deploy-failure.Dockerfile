# Deploy target for the P0-06 deployment-failure drill.
#
# A real systemd host in a container: Ubuntu 24.04, systemd as PID 1, sshd on
# 22. `otter deploy` converges a host over SSH and installs a systemd unit, so
# the only honest way to drill a mid-deploy failure locally is to give it both.
# The container is thrown away at the end of the run; the deploy writes only
# inside /opt/otter and /etc/otter.
#
# It is built by scripts/drill/deploy-failure.sh and run privileged with a
# writable cgroup mount, the same contract as unit-caps.Dockerfile:
#
#   docker run -d --privileged --cgroupns=host \
#     -v /sys/fs/cgroup:/sys/fs/cgroup:rw -p 2222:22 otter-deploy-target
#
# deluser/lock the root password is deliberate: the drill authenticates with a
# key it generates at run time and injects via docker exec, so the image
# carries no credential.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
ENV container=docker

# The tool list mirrors SSH.requiredTools() (internal/deploy/remote.go): a
# deploy fails with its own message when one is missing, which would look like
# a drill bug rather than a real host shortfall.
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      systemd systemd-sysv util-linux procps \
      python3 python3-venv \
      openssh-server rsync sudo iproute2 \
      curl ca-certificates findutils \
 && rm -rf /var/lib/apt/lists/* \
 && rm -f /lib/systemd/system/multi-user.target.wants/* \
      /etc/systemd/system/*.wants/* \
      /lib/systemd/system/local-fs.target.wants/* \
      /lib/systemd/system/sockets.target.wants/*udev* \
      /lib/systemd/system/sockets.target.wants/*initctl* \
      /lib/systemd/system/sysinit.target.wants/systemd-tmpfiles-setup* \
      /lib/systemd/system/systemd-update-utmp-runlevel.service \
 && passwd -l root \
 && mkdir -p /run/sshd \
 && sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config \
 && systemctl enable ssh

EXPOSE 22
STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]
