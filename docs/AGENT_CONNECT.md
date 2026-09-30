# Connect a project to the Control Plane

Service installation maintenance: [updates, daily checks, backups and rollback](UPDATES.md).

Run once in the project that agents will edit:

```sh
./bin/release-control connect \
  --url http://127.0.0.1:8090 \
  --workspace /absolute/path/to/project \
  --product-id PRODUCT_ID
```

The `connect` command does not start a server or open a database. The server must already be running. Omit `--product-id` only when the server contains exactly one Product. The installer checks `/api/agent-kit`, verifies the skill checksum, and checks the selected Product's context before changing files.

## Sign in an independent agent

For an OIDC-protected remote server, first enroll a separate agent:

```sh
/absolute/path/to/release-control login \
  --url https://control.example.test \
  --profile default --name "Codex workstation" \
  --products PRODUCT_ID --scope "read write"

/absolute/path/to/release-control connect \
  --url https://control.example.test \
  --workspace /absolute/path/to/project \
  --product-id PRODUCT_ID --profile default
```

Open the displayed verification URL, sign in through your identity provider,
check the short code, name, Product grants and permissions, and explicitly approve
that agent. The CLI waits for approval and saves an independent persistent agent
credential. Browser logout or expiry does not terminate an approved agent; revoke
it from **Your account → Independent agents** when access should end. Add `deploy`
to the requested scope only when the agent needs deployment permission and your
account is allowed to grant it.

The token is never printed or written to project configuration. It is encrypted
with AES-GCM in a profile bound to the exact server URL, under
`~/.release-control/`. The directory is private (`0700`); encrypted profiles and
the independent per-user encryption key are `0600` files. Symlinks and insecure
permissions are rejected. Keep that directory outside repositories and backups
shared with other users. Encryption protects copied credential records; someone
who can read both the key and records as your OS user can decrypt them.

The generated Codex entry launches the absolute `release-control` executable
with `mcp --url … --profile …`. That subprocess loads its credential on every
start and uses the official MCP SDK for stdio and authenticated Streamable HTTP.
This works after restarting Codex and does not depend on its inherited shell
environment or an active human login. Keep the executable at its installed path,
or pass `connect --executable /absolute/path/to/release-control` for a permanent
installation. Re-run `login` with the same profile to replace a revoked credential.
Remote authentication requires HTTPS; HTTP is accepted only for explicit
loopback URLs (`localhost`, `127.0.0.1`, `::1`).

For an existing environment-managed token, the explicit alternative is:

```sh
./bin/release-control connect \
  --url https://control.example.test \
  --workspace /absolute/path/to/project \
  --product-id PRODUCT_ID --token-env RC_TOKEN
```

`RC_TOKEN` must already be set. This mode saves only the environment variable name
and generates a direct HTTP entry; Codex must inherit that variable when it starts.
Do not combine `--token-env` and `--profile`, and avoid raw credentials in URLs or
shell arguments. Without either flag, `connect` uses the saved default profile
when present and otherwise connects to a local server without a credential.

## Installed files

| File | Purpose |
| --- | --- |
| `.codex/config.toml` | Project-scoped MCP entry: credential-loading stdio subprocess for a saved profile, or direct HTTP for local/environment-token mode. |
| `.agents/skills/rcp-handoff/SKILL.md` | Server-provided handoff skill with context checkpoint instructions. |
| `AGENTS.md` | Managed block binding this project to the Product and instructing agents to resume, record progress, and hand off. |
| `.release-control.json` | Binding: base URL, Product ID, MCP server name and optional credential profile or token environment reference. |

The default MCP server name is `release-control`; change it with `--name` if needed. Existing unrelated TOML settings and AGENTS.md content are preserved. Re-running the command updates only its marked blocks and owned skill/binding. It refuses conflicting unmanaged MCP entries, unmanaged skills, malformed configuration and symlinked installation paths. All output is staged before replacement; ordinary write failures roll back prior replacements. This is not a multi-file crash transaction: an interrupted process can leave temporary staging/backup files.

The installer changes no global Codex configuration, trust settings or global skills. Review the project-local changes with Git before committing them. Binding files contain no token value but may contain a private server URL or Product ID; decide whether they belong in that repository.

## Activate in Codex

Trust the project through Codex's normal trust flow, then **start a new chat or reload MCP configuration**. This command does not retroactively attach tools to an already-running chat.

Each independent Git repository is a project scope. If a workspace contains nested independent repositories, run `connect` for each repository where Codex should load the MCP entry and handoff instructions; a connection at the parent directory may not be inherited across repository boundaries.

Once connected, agents should discover the Product, resume the relevant Feature/Integration before editing, record decisions and verification as they work, and invoke `rcp-handoff` before pausing, finishing or transferring context. The metastore remains authoritative; the installed files are connection instructions, not a replacement state file.
