# Local storage: one binary, no database server

Run from this repository:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/release-control ./cmd/server
RCP_DATA_PATH="$HOME/.local/share/release-control/state.db" /tmp/release-control
```

The UI and HTTP API are at `http://127.0.0.1:8090`; MCP is at `http://127.0.0.1:8090/mcp`.
Without a workspace root, the first run creates an empty instance. Agents create Products and record development state through the same API/MCP commands as a PostgreSQL-backed instance.

To bootstrap from a local workspace:

```sh
RCP_DATA_PATH="$HOME/.local/share/release-control/state.db" \
RCP_WORKSPACE_ROOT="/absolute/path/to/your-product" \
/tmp/release-control
```

Only an empty **local** database is bootstrapped: one Product gets the canonical workspace directory name, then the read-only scanner discovers the initial structure. The audit actor is `system/workspace`. A configured PostgreSQL database is never bootstrapped automatically. Existing Products are never automatically rescanned on restart. If scanning fails, the server logs a warning and keeps the created Product; an agent can retry scanning explicitly. No source files are changed.

The build command strips debug information (`-s -w`) and omits embedded development paths (`-trimpath`). Binary size depends on platform and Go version; neither a separate frontend runtime nor PostgreSQL is needed for local mode.

## Configuration

| Variable | Behavior |
| --- | --- |
| `DATABASE_URL` | When nonempty, use PostgreSQL. Connection errors fail startup; the service never silently switches stores. |
| `RCP_DATA_PATH` | Embedded database file when `DATABASE_URL` is absent. Parent directories are created. |
| `LISTEN_ADDR` | Default `127.0.0.1:8090`. |
| `RC_TOKEN` | Optional shared API/MCP authentication token; configure when exposing the instance. |
| `RCP_WORKSPACE_ROOT` | Optional root for the read-only local workspace scanner. Without it, workspace scanning is disabled. Local filesystem access is not part of exported Product configuration. |

Without `RCP_DATA_PATH`, the file is `release-control/state.db` under Go's `os.UserConfigDir()`: typically `~/.config` on Linux and `~/Library/Application Support` on macOS. The selected local path is logged at startup. Use an explicit path in scripts and service definitions.

The embedded backend is bbolt, a pure-Go transactional database. It stores the same structured domain state, events and operation history as PostgreSQL and uses the same application validation and commands. Commits are atomic and synced to disk. Reads are consistent snapshots. Rejected commands leave the previous state unchanged.

## Container

```sh
docker build -t release-control .
docker run --name release-control --rm \
  -p 127.0.0.1:8090:8080 \
  -v release-control-data:/data \
  -v "$PWD:/workspace:ro" -e RCP_WORKSPACE_ROOT=/workspace \
  release-control
```

The image runs as UID `10001`, includes Git for read-only repository inspection, and defaults `RCP_DATA_PATH` to `/data/state.db`. The named volume survives container removal. Bind-mounted data directories must be writable by UID `10001`. Mount workspaces separately when using the scanner; they do not belong in the database volume.

## Backup, migration and operating limits

Use `create_backup` / `restore_backup` through MCP or the documented HTTP backup endpoints. The compressed format is shared by both backends: local → local, local → PostgreSQL, and PostgreSQL → local. Restore requires an empty destination and never replays external side effects. See [BACKUP.md](BACKUP.md).

For a file-level backup, stop the process before copying `state.db`; copying a live database file is not the supported backup path. Keep the database file and its volume outside source control. Secret values remain external; only references travel in a backup.

One process owns a local file. A second process fails to acquire its lock instead of writing concurrently. Do not share the file between replicas or through network storage. Use PostgreSQL for a shared server deployment. The embedded MVP stores up to 256 MiB of serialized domain state; API backup limits remain those in BACKUP.md. Both modes serve the same UI/API/MCP without a separate frontend deployment.
