# Run Release Control Plane as one binary

Standalone mode needs no Docker, PostgreSQL, or running RCP installation. The binary includes the UI, API, and MCP server. Start it when you need it; the SQLite file remains after it exits.

## Download and verify

Open [GitHub Releases](https://github.com/deadnodes/ai-development-workspace/releases) and choose a `standalone-<sha12>` release. Download `SHA256SUMS` and the binary for your OS and CPU:

| System | Intel/AMD 64-bit | ARM 64-bit |
| --- | --- | --- |
| Windows | `release-control-windows-amd64.exe` | `release-control-windows-arm64.exe` |
| macOS | `release-control-darwin-amd64` | `release-control-darwin-arm64` |
| Linux | `release-control-linux-amd64` | `release-control-linux-arm64` |

A Raspberry Pi running **64-bit** Raspberry Pi OS (`uname -m` prints `aarch64`) uses `release-control-linux-arm64`. No 32-bit builds are provided. Compare the downloaded file's hash with the corresponding entry in `SHA256SUMS`: `Get-FileHash -Algorithm SHA256 <file>` on Windows, `shasum -a 256 <file>` on macOS, or `sha256sum <file>` on Linux. Use only binaries you trust. To build locally instead, use `CGO_ENABLED=0 go build -o ./bin/release-control ./cmd/server` (PowerShell: `$env:CGO_ENABLED = '0'; go build -o .\bin\release-control.exe ./cmd/server`).

## Serve the UI, API, and HTTP MCP

Put the database in a local directory alongside your project, not in Git. In a shell with `DATABASE_URL` unset, use the commands below and open `http://127.0.0.1:8090`. The HTTP MCP endpoint is `/mcp`; `/healthz` is the health check. The optional `--workspace` scans local Git repositories and `AGENTS.md` files and initializes a workspace Product in an empty database. Git is optional unless you want repository inspection. Omit `--workspace` for an empty installation you configure yourself.

**Windows (PowerShell)**

```powershell
$bin = "$HOME\Tools\release-control-windows-amd64.exe"
$project = "$HOME\Projects\My Project"
$db = "$HOME\Projects\My Project.rcp.sqlite"
& $bin serve --data $db --workspace $project
```

**macOS, Linux, and Raspberry Pi** (adjust `target` to match your download)

```sh
target=linux-arm64
bin="$HOME/bin/release-control-$target"
project="$HOME/projects/My Project"
db="$HOME/projects/My Project.rcp.sqlite"
chmod +x "$bin"
"$bin" serve --data "$db" --workspace "$project"
```

Running the binary with no subcommand also starts the HTTP server. With `DATABASE_URL` set, it uses PostgreSQL; unset that variable before an explicit `serve --data`. Without `--data` or `RCP_DATA_PATH`, the default file is `release-control/rcp.sqlite` under Go's user config directory: `%AppData%` on Windows, `~/Library/Application Support` on macOS, or `$XDG_CONFIG_HOME` (usually `~/.config`) on Linux. `LISTEN_ADDR` and `RCP_WORKSPACE_ROOT` are the environment equivalents of `--listen` and `--workspace`.

Windows supports local project tracking, HTTP, and stdio MCP. Native worker sandboxing and secure local OCI publication require a supported Unix host; they are not part of the Windows standalone runtime.

## On-demand MCP for Codex and Claude

`mcp --data` starts its own local application in the client subprocess. It uses stdio, not HTTP; it needs no token, and you do not need to keep `serve` open. Never launch this mode for an untrusted user: access to the file and the ability to start the binary is full admin access. For a remote instance continue to use `mcp --url URL --profile NAME`; `--url` and `--data` cannot be combined.

```sh
"$bin" connect --data "$db" --workspace "$project" --harness both
"$bin" doctor --data "$db" --workspace "$project"
```

```powershell
& $bin connect --data $db --workspace $project --harness both
& $bin doctor --data $db --workspace $project
```

`connect` writes project-level `.codex/config.toml` and `.mcp.json` using absolute paths and separate arguments for `mcp --data PATH --workspace DIR`. Choose `--harness codex` or `--harness claude` to write only that client's config. Open or reload the project in the client and accept its normal project trust and MCP prompts. Review these files before committing: they contain machine-specific paths. `doctor` checks the file and the client configuration without launching a server or creating the database.

## Access and backups

- **Single local admin.** With SQLite and no `RC_TOKEN` or OIDC configuration, HTTP requests from loopback have full access, including to private Products. Bind locally by default (`127.0.0.1:8090`). A non-loopback bind without authentication fails before opening the database.
- **LAN access is opt-in.** Set a strong, unique `RC_TOKEN`, set `ALLOWED_HOSTS` to the hostname or LAN IP clients use, and start with `--listen 0.0.0.0:8090` (or a specific interface). Set the token in the UI's Settings or send it as `Authorization: Bearer …` for API/MCP. Use a firewall and a TLS reverse proxy if traffic crosses a network; plain HTTP does not protect the bearer token. For multi-user identity and private-Product isolation, use [OIDC authentication](AUTHENTICATION.md), preferably with PostgreSQL.
- **Shared file, same computer.** SQLite uses WAL, foreign keys, a 10-second busy timeout, and `synchronous=NORMAL` (transactions survive process crashes; the newest commits can be lost on sudden power failure). `serve` and several stdio clients can use the same database concurrently on a local disk. Do not use SQLite WAL over SMB/NFS, cloud-sync folders, or across hosts. To move a database to a neighboring PC, back it up first and copy the backup to that PC's local disk.
- **Back up before moving or upgrading.** Use MCP `create_backup` for the portable logical archive; restore into an empty instance with `restore_backup`. See [Backup](BACKUP.md) for limits and what is excluded. With SQLite tooling installed, run `sqlite3 "$db" '.backup backup.sqlite'` for a consistent live file backup. To copy the main file directly, close HTTP and every MCP process using it, then copy it. Do not copy only the main file from a running installation: recent commits can still be in its `-wal` sidecar. Protect backup files as you protect the database. Old bbolt files are rejected; use a fresh SQLite path (no in-place migration).

## Optional autostart

Use autostart only when you want the UI/API available continuously. For MCP-only use, let the client start the binary. Use absolute paths for the binary, database, and workspace, and run the service as the account that owns those directories.

- **Windows Task Scheduler:** Create a task triggered **At log on** for your user. Choose **Start a program**, set **Program/script** to the absolute `.exe` path and **Add arguments** to `serve --data "C:\path with spaces\project.sqlite" --workspace "C:\path\project"`. Set the task not to start another instance if already running and disable the default execution time limit for a long-running server.
- **macOS launchd:** Save a user agent at `~/Library/LaunchAgents/com.releasecontrol.local.plist`. Set `Label` to `com.releasecontrol.local`, `RunAtLoad` to `true`, and `ProgramArguments` to an array of separate strings: absolute binary path, `serve`, `--data`, absolute database path, `--workspace`, absolute project path. Load it with `launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.releasecontrol.local.plist"`.
- **Linux / Raspberry Pi systemd:** Save a user service at `~/.config/systemd/user/release-control.service`:

  ```ini
  [Unit]
  Description=Release Control Plane

  [Service]
  ExecStart=/home/you/bin/release-control serve --data "/home/you/projects/My Project.rcp.sqlite" --workspace "/home/you/projects/My Project"
  Restart=on-failure

  [Install]
  WantedBy=default.target
  ```

  Run `systemctl --user daemon-reload` and `systemctl --user enable --now release-control`. A user service normally starts with the user session.
