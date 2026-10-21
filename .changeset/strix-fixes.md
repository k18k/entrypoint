---
"entrypoint": patch
---

More fixes from the Strix security review:

- Revoked tokens stay rejected when a new configuration snapshot is applied.
- A rollback with no previously applied revision applies an empty authorization snapshot instead of keeping the failed one.
- Route prefixes conflict case-insensitively (`/API` and `/api` on one host are rejected).
- `entrypoint backup` requires a password of at least 12 characters from every source (flag, file, prompt).
- The connector rejects `{{` and `}}` in `--health`, `--upstream`, `--service` and the gateway name from the bundle.
