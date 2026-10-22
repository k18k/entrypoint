---
"entrypoint": minor
---

Web panel: `entrypoint install --panel-name panel.example.com` serves it through Caddy with an automatic certificate. Sign-in is with passkeys only (WebAuthn: a password manager such as Proton Pass, or a security key such as a YubiKey): `entrypoint panel register` prints a one-time link valid for 60 seconds that adds one, and `entrypoint panel keys` lists and revokes them. From the panel you create an app with its hosts in one step and get its connection bundle with ready commands for a Linux machine, the Node.js SDK or Docker Compose; manage routes, tokens and VPN machines; issue VPN keys; and check DNS records and revisions. Sessions use an HttpOnly, Secure, SameSite=Strict cookie, changes need a CSRF token and the panel's Origin, and failed sign-ins are limited per IP address.
