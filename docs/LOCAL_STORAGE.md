# Local SQLite storage

See [Standalone quickstart](STANDALONE.md) for installation, on-demand MCP, access, and backups.

- `DATABASE_URL` selects PostgreSQL. Otherwise `--data` or `RCP_DATA_PATH` selects SQLite. If neither is set, SQLite uses `os.UserConfigDir()/release-control/rcp.sqlite`.
- A standalone server and local MCP processes can share the same file on one machine. To serve multiple hosts or replicas, use PostgreSQL rather than a network-mounted SQLite file.
- The image defaults to `/data/rcp.sqlite` when PostgreSQL is not configured. Mount `/data` on a persistent volume writable by UID `10001`. The image binds on `:8080`; SQLite requires authentication for that bind. See [Docker setup](DOCKER_LOCAL.md).
- Both backends serve the same application and use the same [portable logical backup](BACKUP.md). To back up an OIDC installation, also protect its separate authentication store and encryption key.
