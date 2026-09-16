# Connect a project to the Control Plane

Run once in the project that agents will edit:

```sh
./bin/release-control connect \
  --url http://127.0.0.1:8090 \
  --workspace /absolute/path/to/project \
  --product-id PRODUCT_ID
```

The `connect` command does not start a server or open a database. The server must already be running. Omit `--product-id` only when the server contains exactly one Product. The installer checks `/api/agent-kit`, verifies the skill checksum, and checks the selected Product's context before changing files.

For a server requiring authentication, put its token in the process environment and pass the **variable name**:

```sh
./bin/release-control connect \
  --url https://control.example.test \
  --workspace /absolute/path/to/project \
  --product-id PRODUCT_ID \
  --token-env RC_TOKEN
```

`RC_TOKEN` must already be set. Its value is used to validate the connection but is never written to project files. The Codex process must also inherit that environment variable when it starts. Avoid putting raw credentials in the URL or shell arguments.

## Installed files

| File | Purpose |
| --- | --- |
| `.codex/config.toml` | Project-scoped HTTP MCP entry pointing to `/mcp`; token environment reference when configured. |
| `.agents/skills/rcp-handoff/SKILL.md` | Server-provided handoff skill with context checkpoint instructions. |
| `AGENTS.md` | Managed block binding this project to the Product and instructing agents to resume, record progress, and hand off. |
| `.release-control.json` | Binding: base URL, Product ID, MCP server name and optional token environment reference. |

The default MCP server name is `release-control`; change it with `--name` if needed. Existing unrelated TOML settings and AGENTS.md content are preserved. Re-running the command updates only its marked blocks and owned skill/binding. It refuses conflicting unmanaged MCP entries, unmanaged skills, malformed configuration and symlinked installation paths. All output is staged before replacement; ordinary write failures roll back prior replacements. This is not a multi-file crash transaction: an interrupted process can leave temporary staging/backup files.

The installer changes no global Codex configuration, trust settings or global skills. Review the project-local changes with Git before committing them. Binding files contain no token value but may contain a private server URL or Product ID; decide whether they belong in that repository.

## Activate in Codex

Trust the project through Codex's normal trust flow, then **start a new chat or reload MCP configuration**. This command does not retroactively attach tools to an already-running chat.

Each independent Git repository is a project scope. If a workspace contains nested independent repositories, run `connect` for each repository where Codex should load the MCP entry and handoff instructions; a connection at the parent directory may not be inherited across repository boundaries.

Once connected, agents should discover the Product, resume the relevant Feature/Integration before editing, record decisions and verification as they work, and invoke `rcp-handoff` before pausing, finishing or transferring context. The metastore remains authoritative; the installed files are connection instructions, not a replacement state file.
