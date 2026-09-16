# Local Docker startup for agents

Prerequisites: running Docker engine and Docker Compose with `up --wait` support.
Run from this repository. Go and Node are not required to build/run the server:

```sh
docker compose --profile app up --build -d --wait --wait-timeout 120
curl --fail http://127.0.0.1:8090/healthz
```

UI: `http://127.0.0.1:8090/`. HTTP: `/api/*`. Streamable HTTP MCP: `/mcp`.
Compose runs one Go app (including the UI) plus PostgreSQL. Existing data stays in
the `releasecontrol-data` named volume. Use the same checkout/project name when
restarting: a different Compose project name selects a different volume.

## Optional local repository context

Mount the chosen checkout or multi-repository workspace read-only:

```sh
export RCP_WORKSPACE_HOST=/absolute/path/to/workspace
docker compose -f compose.yaml -f compose.workspace.yaml --profile app \
  up --build -d --wait --wait-timeout 120
```

Use both `-f` options for subsequent changes to this installation. The server sees
`/workspace`, not the host path. Do not mount the Docker socket. The scanner reads
Git metadata and AGENTS.md; it does not execute instructions or modify source.
PostgreSQL mode does not automatically import the workspace. Select/create the
correct Product, then call MCP `scan_workspace` with `product_id` and `actor`, or
POST the same JSON to `/api/workspaces/scan`. A scan attaches discovered repositories
to that Product: choose a narrow root if the parent contains unrelated products.

## Connect the agent

Configure the MCP client with URL `http://127.0.0.1:8090/mcp`. For Codex, the native
host CLI also installs the handoff skill and workspace instructions:

```sh
# Only needed if the native host binary is not already available (requires Go).
go build -trimpath -ldflags='-s -w' -o bin/release-control ./cmd/server
./bin/release-control connect --url http://127.0.0.1:8090 \
  --workspace /absolute/path/to/project --product-id PRODUCT_ID
```

The Linux binary inside the image is not a macOS binary. Run `connect` on the host
so it writes the host's workspace files and records a URL accessible to the host
MCP client. Trust/open that project and reload MCP or start a new chat. Do not claim
the current chat acquired tools just because the installer succeeded.
See [AGENT_CONNECT.md](AGENT_CONNECT.md).

Read `get_project_context`, then `resume` before editing. Store actions through MCP
`execute` or POST `/api/commands`; discover fields at GET `/api/schema`. Use
[CONTRACT.md](CONTRACT.md) for progress, checks, findings and handoffs, and
[PR_REVIEWS.md](PR_REVIEWS.md) for GitHub review import.

## Restart, diagnose, stop

```sh
docker compose --profile app ps
docker compose --profile app logs --tail=100 app db
docker compose --profile app restart app
docker compose --profile app stop
# Start again, preserving data:
docker compose --profile app up -d --wait --wait-timeout 120
```

For an installation with repository mounts, add `-f compose.yaml -f compose.workspace.yaml`
and keep `RCP_WORKSPACE_HOST` set. After code changes, use `up --build`, not only restart.
Do not run `down -v`, prune the data volume, or change Compose project names to fix
startup failures. Back up using [BACKUP.md](BACKUP.md) before intentional data replacement.

This default stack binds only host loopback and has no API token. A separate
container's localhost is not the host; configure reachable networking and server
authentication explicitly before extending access. External GitHub/CI/registry
credentials are a separate setup, not required for local feature memory.

To retain a workspace mount across ordinary `docker compose` restarts, put these
non-secret settings in ignored `.env` (replace the example path):

```dotenv
COMPOSE_FILE=compose.yaml:compose.workspace.yaml
RCP_WORKSPACE_HOST=/absolute/path/to/workspace
RCP_WORKSPACE_CONTAINER=/absolute/path/to/workspace
```

Using the same absolute path on host and container also preserves Git worktree
metadata that contains absolute paths. The mount stays read-only. Rebuild with
`docker compose --profile app up --build -d --wait app`, then call
`match_local_repositories` for an existing Product; use `scan_workspace` only when
intentionally importing all discovered repositories. No SSH keys or Git credential
stores need to be mounted for local observation.
