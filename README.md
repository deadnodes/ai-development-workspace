# Release Control Plane

A Go modular monolith for human developers and coding agents. Git stores source history; Release Control Plane stores development intent, integration plans, decisions, discoveries, verification evidence and handoffs.

## Run locally

Requires Go 1.25+, Docker Compose and Node.js for the JavaScript syntax check. PostgreSQL is the only database implementation.

```sh
make db
make run
```

Open **http://127.0.0.1:8090**. The same process serves `/`, `/api/*` and `/mcp`. Start with **Create product**, then add a feature and integrations. No demo data is silently inserted. PostgreSQL data survives app and container restarts in a named volume.

Alternatively, run the application and database in containers:

```sh
docker compose --profile app up --build -d
```

Do not run the container app and local app on port 8090 simultaneously.

## Interfaces

- `GET /api/state` — full workspace state and audit history.
- `GET /api/features/{id}/context` — deterministic agent resume package.
- `GET /api/schema` — discover command inputs.
- `POST /api/commands` — one attributed engineering action.
- `/mcp` — official Go SDK Streamable HTTP server, with `get_state`, `resume`, `execute` tools. Tools advertise JSON schemas; execute shares the HTTP/UI application service.

```sh
curl -s http://127.0.0.1:8090/api/commands \
  -H 'Content-Type: application/json' \
  -d '{"action":"create_product","actor":"human/developer","data":{"name":"Payments","description":"Payment platform"}}'
```

Use the returned product ID as top-level `product_id` in `create_feature`; use the returned feature ID in `create_integration`. See [command contract](docs/CONTRACT.md) for the entire workflow.

Configure any Streamable HTTP MCP client with URL `http://127.0.0.1:8090/mcp`. If `RC_TOKEN` is configured, send `Authorization: Bearer <token>`. A new agent should first call `get_state`, select a feature and call `resume` with `feature_id`. `execute` can record a structured `handoff` with completed/current/remaining/next/warnings so the next agent does not need conversation history.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | local Compose database on port 55432 | PostgreSQL connection string |
| `LISTEN_ADDR` | `127.0.0.1:8090` | HTTP listener |
| `RC_TOKEN` | unset | Optional shared bearer token for API/MCP |
| `ALLOWED_HOSTS` | localhost and loopback addresses | Additional comma-separated HTTP hostnames |
| `TEST_DATABASE_URL` | unset | Enable real PostgreSQL transport/persistence tests |

This MVP is a trusted workspace application. Actor attribution is not authentication or RBAC. It binds loopback by default, rejects cross-origin requests and unexpected Host headers, and limits command sizes. Use a token and TLS ingress before intentionally sharing it. The UI can supply the configured token. No Git provider, Flux, Kubernetes or production mutations are performed.

## Development

```sh
make check
TEST_DATABASE_URL='postgres://releasecontrol:releasecontrol@localhost:55432/releasecontrol?sslmode=disable' go test -race ./...
```

Tests using PostgreSQL isolate their data. `make check` runs Go formatting checks, vet, JavaScript syntax and renderer checks, race tests and binary build. The frontend is embedded directly: no node_modules or separate build/deployment needed.

## Semantics and scope

Integrations are independently implementable/releasable. Completing an integration makes it **ready**, not deployed. Blocking gates require passing latest check results and no applicable open findings or blockers. Resolving a finding preserves failed evidence; rerun the check. Results, structured handoffs and events preserve provenance. Release plans are immutable records of intent; they do not claim a production deployment.

Environment desired, reconciled and runtime state are distinct. Unknown means unknown. Repository/branch/commit references and desired integration composition are modelled; provider interfaces are prepared for Git/Flux/Kubernetes adapters. Automated branch composition, executors, production promotion, RBAC and multi-tenancy are deliberately outside this slice.

To seed the project’s own tracked bootstrap feature explicitly, run `node scripts/dogfood.mjs`. See [validation evidence](docs/VALIDATION.md).

See [architecture and invariants](docs/ARCHITECTURE.md), [shared contract](docs/CONTRACT.md), and [bootstrap handoff](docs/IMPLEMENTATION_STATE.md). Once dogfooding is seeded, the application feature and handoff replace the bootstrap file as the development state authority.
