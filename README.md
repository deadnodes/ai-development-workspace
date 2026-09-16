# Release Control Plane

Local startup needs only the Go binary: with `DATABASE_URL` unset, it uses embedded storage (`RCP_DATA_PATH`). PostgreSQL remains optional. Configure `RCP_WORKSPACE_ROOT` for checkout/AGENTS scanning; see [local workspace and portable project context](docs/LOCAL_WORKSPACE.md).

A development metastore and control plane: intent, integrations, commit provenance, verification, agent context and delivery orchestration. It does not edit managed application source code or business data. See [responsibility boundary](docs/RESPONSIBILITY_BOUNDARY.md).

Go modular monolith. One process serves the embedded UI, HTTP API and MCP; embedded bbolt or configured PostgreSQL stores state and audit history.

One-command workspace setup: `./bin/release-control connect --url http://127.0.0.1:8090 --workspace /PATH/TO/PROJECT --product-id PRODUCT_ID`. Installs project MCP configuration, instructions and the handoff skill; see [agent connection](docs/AGENT_CONNECT.md).

**Agent entry point: [docs/AGENT_QUICKSTART.md](docs/AGENT_QUICKSTART.md).** Start there for MCP connection, exact setup commands, GitHub App credentials, repository attachment and the first DEV operation.

## Product-independent by design

A fresh instance contains no company configuration. With a configured workspace root, the first local startup discovers repositories and creates a Product named after that workspace. Add any number of unrelated Products, each with its own components, repository selections, environments and external dependencies. Provider connections and the Repository Registry are instance-level; Products receive explicit access and attachments. No organization, repository name, team or GitOps layout is built into the runtime.

GitHub is the first implemented provider stack, not a product identity. The current deployment executor supports explicit DEV policies; arbitrary environment names are supported by the domain. Credentials and instance data stay outside source control. The optional `scripts/dogfood.mjs` seeds historical context for this project only and never runs automatically.

See [contribution and publication notes](CONTRIBUTING.md).

## Kubernetes: one instance per product

Deploy a dedicated Control Plane and database per product. All developers, agents and environments of that product share its instance. Generic Deployment/Service manifests and agent setup instructions: [Kubernetes guide](docs/KUBERNETES.md). Multi-product instances remain supported.

## Start

Requires Docker with Compose. From the repository root:

```sh
docker compose --profile app up --build -d
curl --fail http://127.0.0.1:8090/healthz
```

- UI: `http://127.0.0.1:8090`
- MCP: `http://127.0.0.1:8090/mcp` (Streamable HTTP)
- API state: `GET /api/state`
- Command schemas: `GET /api/schema`
- Commands: `POST /api/commands`

First agent calls: `get_state {}`, `get_project_context {"product_id":"…"}`, then `resume {"feature_id":"…"}`. Fresh databases contain no demo data. Record a structured `handoff` before stopping work.

The default Compose setup is loopback-only, without an API token. PostgreSQL data survives restarts in a named volume. `docker compose --profile app down` stops the services; do not add `-v` unless intentionally deleting data.

For local development, use `make run` (Go 1.25+), optionally setting `RCP_DATA_PATH` and `RCP_WORKSPACE_ROOT`. To use PostgreSQL, run `make db` and supply `DATABASE_URL` explicitly. Do not run both app modes on port 8090 simultaneously.

## Current execution boundary

Implemented: shared Repository Registry, Product/Feature/Integration scopes, unmanaged external dependencies, GitHub App authentication, Git observation, asynchronous Actions/GHCR operations and explicit DEV GitOps changes. The server does not use the local `gh` token. GitHub credentials and workflow/mapping configuration are required.

A successful GitOps operation records desired state and remains pending reconciliation. Flux/Kubernetes runtime observation, automatic branch composition and production deployment are not implemented. Local tests are not evidence of a successful live deployment.

## References

- [Agent quickstart](docs/AGENT_QUICKSTART.md) — run, connect, configure, deploy, resume.
- [GitHub build and GitOps contract](docs/GITHUB_DEV.md) — workflow, private GHCR and evidence details.
- [Application commands](docs/CONTRACT.md) — shared HTTP/MCP/domain contract.
- [Architecture](docs/ARCHITECTURE.md) and [roadmap](docs/ROADMAP.md).
- [Validation](docs/VALIDATION.md) — tested behavior and live acceptance limits.

## Checks

With Compose database running, Go 1.25+, Node.js and Python 3:

```sh
TEST_DATABASE_URL='postgres://releasecontrol:releasecontrol@localhost:55432/releasecontrol?sslmode=disable' make check
```

Runs formatting, vet, frontend checks, workflow-contract tests, race-enabled Go/PostgreSQL tests and binary build. No frontend dependency install or separate deployment is needed.

## Configuration authority

Project configuration and history are persisted in PostgreSQL. UI/API/MCP are the primary interface; Git can hold a generated, one-way configuration mirror. Containers do not import project state from Git on startup. See [configuration authority and export](docs/CONFIGURATION_AUTHORITY.md).

## Backup and migration

Export/import the complete instance history as gzip through API/MCP or `scripts/backup.mjs`. Restore is atomic and requires an empty destination; active operations are cancelled with original evidence retained. See [agent backup and migration guide](docs/BACKUP.md).

Codex P1/P2 review comments can become persistent findings through MCP `sync_pull_request_review`. Shared PRs use preview and explicit comment selection. See [PR review workflow](docs/PR_REVIEWS.md).

## Shared testing, independent release

Test selected features together on a versioned environment composition. Release selected ready integrations through main, then build production artifacts from pinned main commits. DEV composition images are not production promotion inputs. See [scenario and release design / implementation status](docs/TEST_AND_RELEASE_FLOW.md).

Frontend verification uses test-only jsdom. Run `npm ci` once before `make check`; the shipped UI remains dependency-free plain JavaScript. See [live UI updates](docs/LIVE_UI.md).
