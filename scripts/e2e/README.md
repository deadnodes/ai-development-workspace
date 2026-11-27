# Live RCP E2E scripts (dev)

Run against dev RCP with real device-flow logins. They need:

- Node 24 and Playwright (`PW_DIR` points at the directory that has `playwright` in `node_modules`);
- `RC_BIN`, the `release-control` binary built from this repository;
- a credentials file with mode 600. Build it from `scripts/tofu keycloak-dev output -json e2e_credentials` in the workspace terraform repository, and delete it after the run. Never print it.

| Script | Checks |
|---|---|
| `user-isolation.mjs <ids.json> <creds.json>` | Stage 1. Two humans with their own agents. Claim, takeover, heartbeat and progress on another user's work return `NOT_ASSIGNEE`. |
| `private-products.mjs <creds.json>` | Stage 2. A private product is visible only to its owner and invited members. Writes and member management are refused for everyone else. |

`RCP_URL` overrides the default dev URL.
