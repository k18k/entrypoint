---
"entrypoint": minor
---

Per-IP connection limits on the public side, as kernel rules (nftables) that the installer manages in a table of its own: at most 256 concurrent connections per client address on 80/443 (IPv6 per /64) and 100 new connections per second on 80/443 and the tunnel port. Each service is capped at 1024 concurrent requests in Caddy, so one service cannot take all of frps. Set them with `--per-ip-connections`, `--per-ip-rate` and `--service-connections` (0 turns a limit off). `install.sh` installs nftables when it is missing and apt is available; otherwise the installer warns and the per-IP limits are off.
