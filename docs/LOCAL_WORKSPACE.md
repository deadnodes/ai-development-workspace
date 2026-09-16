# Local workspace and portable project context

The same Go binary serves the UI, API and MCP. With no `DATABASE_URL`, it uses an embedded database at `RCP_DATA_PATH`; the default is `os.UserConfigDir()/release-control/state.db`. For example, on macOS this is beneath the user’s Library/Application Support directory. Docker uses `/data/state.db`; keep that directory on a persistent volume. Set `DATABASE_URL` to use PostgreSQL instead. The UI and application commands are identical with either store.

```sh
go build -o bin/release-control ./cmd/server
RCP_DATA_PATH="$PWD/.local/state.db" RCP_WORKSPACE_ROOT="$PWD" ./bin/release-control
```

Ensure `DATABASE_URL` is unset to select embedded storage. The workspace root is optional. Without it, remote repository context and project knowledge still work, while scanning explicitly reports unavailable. In Docker, mount the intended workspace and set `RCP_WORKSPACE_ROOT` to its path **inside the container**. The browser’s filesystem is not the server’s filesystem.

## Orient a human or a fresh agent

Open **Local workspace & project context** on the Product overview. Project context includes:

- Product repositories and remote URLs, components and environments;
- curated overview, agent instructions, non-secret parameters, nested areas and relationships/contracts;
- local checkout observations with relative paths, branches, commits, AGENTS.md text/hash/truncation markers and errors;
- workspace AGENTS.md and any imported repository document context.

AGENTS content is displayed as escaped text. Scanning does not execute it. Imported repository documents describe remote context; they do not assert that the repository exists locally. Agents can use the remote URLs in their own work environment; the control plane does not clone repositories for them.

**Scan configured workspace** reads only the configured server root. The request accepts Product and actor, not an arbitrary filesystem path. Scan results are observations, not live monitoring or proof of deployed code. Review errors and truncation before using the context. No scan action builds, publishes or deploys.

## Curated project knowledge

**Edit project knowledge** provides ordinary text fields for overview and instructions. Areas and relationships use JSON arrays in this first version; parameters use a JSON object of string values. Saving replaces the current curated document and records an audit event; it does not overwrite checkout history or feature handoffs. Keep credentials and business data out of project parameters and instructions. Components and environments can also carry non-secret `parameters` string maps through their shared creation commands; project context displays these and portable configuration preserves them. These are descriptive settings, not deployment execution inputs.

```json
{
  "overview": "API and SDK for the payment service",
  "instructions": "Preserve API v1 compatibility",
  "parameters": {"default_test_environment": "DEV"},
  "areas": [
    {"id": "payments", "name": "Payments", "description": "Payment processing", "repository_ids": ["API_REPOSITORY_ID"]},
    {"id": "sdk", "name": "Client SDK", "parent_id": "payments", "description": "Reusable client package", "repository_ids": ["SDK_REPOSITORY_ID"]}
  ],
  "relationships": [
    {"from": "sdk", "to": "payments", "type": "CONSUMES", "contract": "Keep API v1 callback format compatible"}
  ]
}
```

Use actual Product repository IDs in areas; parent and relationship references use area IDs. Relationship types are finite: DEPENDS_ON, CONSUMES, PROVIDES_TO and SHARES_DATA_WITH.

## Portable configuration

Export creates a portable JSON configuration for review/download. Import creates a new Product preserving configured IDs and rejects conflicts; it is not an upsert. It does not clone source or prove local checkout availability. Scan the destination instance’s configured root to establish actual local observations.

Configuration export/import is distinct from full backup/restore. Use [backup tools](BACKUP.md) when development history and verification evidence must be transferred. Inspect exported instructions/documents before sharing them.

| HTTP | MCP |
| --- | --- |
| `GET /api/products/{id}/context` | `get_project_context` |
| `POST /api/workspaces/scan` with product_id, actor | `scan_workspace` |
| `POST /api/products/{id}/knowledge` with actor, knowledge | `set_project_knowledge` |
| `GET /api/products/{id}/workspace-config` | `export_workspace_configuration` |
| `POST /api/workspaces/import` with actor, configuration | `import_workspace_configuration` |

All mutations retain actor attribution. Actor names are workspace attribution, not an RBAC boundary. Filesystem scan permissions come from the server process and configured root.

## Match existing remote repositories to local copies

For a Product already configured from GitHub, use MCP
`match_local_repositories({product_id, actor})` or
`POST /api/workspaces/match` with the same fields. Unlike `scan_workspace`, this
operation attaches observations only to repositories already selected for that
Product. It never imports unrelated workspace repositories. SSH and HTTPS clone
URLs are matched by host and repository path, not by the local directory name.
Registered IDs, provider bindings, roles and canonical clone URLs are preserved.
Several worktrees can reference one repository. Ambiguous matches are reported
instead of guessed. Scanning first visits immediate workspace repositories so
large temporary trees cannot hide ordinary top-level copies; deeper traversal
remains bounded and reports truncation.

Read `get_project_context` afterward. Each `local_checkouts` record has an ID,
repository ID, workspace root, relative path, branch, commit and readable root
`AGENTS.md` with provenance. These are observations, not permission to execute
repository instructions. Missing or unsafe paths are never treated as available.
Server paths are usable by an agent only when it has access to the same filesystem.
Remote agents should clone the returned repository URLs into their own workspace.
