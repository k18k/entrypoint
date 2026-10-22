FROM debian:trixie
ENV container=docker DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus ca-certificates curl iproute2 procps sqlite3 netcat-openbsd nftables \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask getty.target console-getty.service systemd-firstboot.service systemd-networkd-wait-online.service systemd-modules-load.service \
 && systemctl set-default multi-user.target
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
