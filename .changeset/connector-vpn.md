---
"entrypoint": minor
---

`entrypoint-connector vpn` joins the gateway's VPN without Tailscale or root: `--name vatro-postgres --expose 5432` publishes a local port as `vatro-postgres:5432`, and `--forward vatro-postgres:5432` makes it reachable on `127.0.0.1:5432` of another machine. The bundle (`ENTRYPOINT_VPN`) comes from the panel or `entrypoint vpn key --bundle`. `install-connector.sh` (a release file) installs the connector on a Linux machine and runs it as a systemd service, and the connector image now takes the mode as its command (`tunnel` stays the default). VPN machines resolve each other by name (MagicDNS, `<name>.ep.internal`).
