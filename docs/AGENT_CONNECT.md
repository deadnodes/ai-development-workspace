# Connect a project to the Control Plane

Service installation maintenance: [updates, daily checks, backups and rollback](UPDATES.md).

Run once in the project that agents will edit:

```sh
./bin/release-control connect \
  --url http://127.0.0.1:8090 \
  --workspace /absolute/path/to/project \
  --product-id PRODUCT_ID
```

The `connect` command does not start a server or open a database. The server must already be running. Without `--product-id`, automatic selection uses only the existing bounded MCP `get_state {"limit":2}` query. It requires exactly one valid Product, Product scope with no foreign parents, a present `has_more:false` and consistent count/cursor metadata. Unavailable, ambiguous or malformed discovery requires an explicit Product ID; there is no `/api/state` fallback. The installer checks `/api/agent-kit`, validates both fixed skill names, frontmatter and content checksums, then checks the selected Product's scoped context identity before any file writes. Explicit Product selection skips listing and retains the 2-MiB scoped context response limit. Duplicate JSON fields and invalid encoding in discovery or context are refused before installation.

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
| `.agents/skills/rcp-handoff/SKILL.md` | Canonical handoff/checkpoint skill for project agents. |
| `.agents/skills/rcp-delivery/SKILL.md` | Distinct Feature-delivery skill preserving repository adoption and existing execution policy. |
| `.claude/skills/rcp-handoff/SKILL.md` | The same canonical handoff skill for the project skill directory. |
| `.claude/skills/rcp-delivery/SKILL.md` | The same canonical delivery skill for the project skill directory. |
| `AGENTS.md` | Managed block binding this project to the Product and instructing agents to resume, record progress, and hand off. |
| `.release-control.json` | Binding: base URL, Product ID, MCP server name and optional credential profile or token environment reference. |

The default MCP server name is `release-control`; change it with `--name` if needed. Existing unrelated TOML settings and AGENTS.md content are preserved. Re-running the command updates only its marked blocks and owned skills/binding. All seven target paths are fixed by the installer, never supplied by the server. An unmanaged raw skill may be adopted only when its bytes exactly match the incoming canonical snapshot; differing user content refuses the entire installation. It also refuses conflicting unmanaged MCP entries, malformed configuration and symlinked installation paths, while preserving existing file modes. All output is staged before replacement; ordinary write failures roll back prior replacements. This is not a multi-file crash transaction: an interrupted process can leave temporary staging/backup files.

The `.claude/skills` copies do not configure Claude MCP. The installer changes no Claude settings/MCP configuration, global Codex configuration, trust settings or global skills. Review the project-local changes with Git before committing them. Binding files contain no token value but may contain a private server URL or Product ID; decide whether they belong in that repository.

## Activate in Codex

Trust the project through Codex's normal trust flow, then **start a new chat or reload MCP configuration**. This command does not retroactively attach tools to an already-running chat.

Each independent Git repository is a project scope. If a workspace contains nested independent repositories, run `connect` for each repository where Codex should load the MCP entry and both workflow skills; a connection at the parent directory may not be inherited across repository boundaries.

Once connected, agents should discover the Product, resume the relevant Feature/Integration before editing, record decisions and verification as they work, use `rcp-handoff` for checkpoints/pause/transfer, and use `rcp-delivery` to inspect or deliver a Feature within its existing repository adoption, human approval and execution boundaries. The metastore remains authoritative; the installed files are connection instructions, not a replacement state file.


Automatic discovery uses a separate same-origin HTTP client with no redirects,
reconnect retries or standalone SSE and a 15-second context/HTTP timeout. Its
complete response body is capped at 128 KiB for every status/content type,
including errors and unknown/chunked lengths; an individual SSE event has the
same cap. Oversize is an error, not silent truncation. Raw SSE message JSON is
validated before SDK decoding can overwrite duplicate fields; supported framing
includes multiline data, LF/CRLF/bare CR, an initial BOM, comments and EOF.
The service exposes both fixed skills via `get_agent_skill` and fixed MCP
resources with SHA-256 metadata. Public snapshot repository/commit/path/checksum
provenance is tracked in `internal/agentguide/skills/provenance.json`.
