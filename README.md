# Release Control Plane

Go modular monolith for development intent, integrations, verification, handoffs and DEV delivery. One process serves the embedded UI, HTTP API and MCP; PostgreSQL stores state and audit history.

**Agent entry point: [docs/AGENT_QUICKSTART.md](docs/AGENT_QUICKSTART.md).** Start there for MCP connection, exact setup commands, GitHub App credentials, repository attachment and the first DEV operation.

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

First agent calls: `get_state {}`, then `resume {"feature_id":"…"}`. Fresh databases contain no demo data. Record a structured `handoff` before stopping work.

The default Compose setup is loopback-only, without an API token. PostgreSQL data survives restarts in a named volume. `docker compose --profile app down` stops the services; do not add `-v` unless intentionally deleting data.

For development outside the app container, use `make db` then `make run` (Go 1.25+). Do not run both app modes on port 8090 simultaneously.

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
