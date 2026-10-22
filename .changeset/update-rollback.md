---
"entrypoint": minor
---

`entrypoint update` updates the gateway in place: it checks the release's Ed25519 signature, turns on maintenance mode (configuration changes are refused, token revocations still work), copies the database, runs the new version's migrations, switches versions, restarts the services in order and watches them for 60 seconds. If anything fails it rolls back to the previous version and database and keeps every revocation made meanwhile. `entrypoint maintenance on|off` controls maintenance mode by hand. Releases now carry a signed `SHA256SUMS`, which `install.sh` verifies too (it needs openssl).
