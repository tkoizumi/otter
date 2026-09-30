# systemd host for the P0-04 unit-caps drill.
#
# A systemd-in-container image, so the generated unit can be started on a real
# PID 1 and its cgroup caps observed without a Linux workstation. It is built
# by scripts/drill/unit-caps.sh and is the committed replacement for the ad-hoc
# otter-systemd-test image that no one could rebuild.
#
# Run it privileged with a writable cgroup mount; the drill does this for you:
#
#   docker run -d --privileged --cgroupns=host \
#     -v /sys/fs/cgroup:/sys/fs/cgroup:rw otter-systemd-caps
#
# python3 is what the deliberately runaway job runs under (the runtime's
# external-Python mode). The rest is the systemd base plus the tools the drill
# uses to install and observe the unit.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
ENV container=docker

RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      systemd systemd-sysv util-linux procps \
      python3 rsync curl ca-certificates findutils \
 && rm -rf /var/lib/apt/lists/* \
 && rm -f /lib/systemd/system/multi-user.target.wants/* \
      /etc/systemd/system/*.wants/* \
      /lib/systemd/system/local-fs.target.wants/* \
      /lib/systemd/system/sockets.target.wants/*udev* \
      /lib/systemd/system/sockets.target.wants/*initctl* \
      /lib/systemd/system/sysinit.target.wants/systemd-tmpfiles-setup* \
      /lib/systemd/system/systemd-update-utmp-runlevel.service

STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]
