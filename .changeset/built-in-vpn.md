---
"entrypoint": minor
---

Built-in VPN: `entrypoint install --vpn-name vpn.example.com` runs Headscale behind Caddy, with its embedded DERP relay, and joins the gateway as a node. Routes can then point at any machine in the VPN (`--to 100.64.0.2:3000`). `entrypoint vpn key` issues join keys for the regular Tailscale client, and `vpn nodes` and `vpn rm` manage machines. Backups include the VPN state. With the VPN the gateway also needs UDP 3478 and 41641 open.
