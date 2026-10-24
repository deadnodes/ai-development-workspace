# AI Development Workspace

## Shared authenticated installations

Use [authentication and independent agent enrollment](docs/AUTHENTICATION.md)
for a shared UI/API/MCP endpoint. Human login uses environment-configured OIDC;
agents enroll through device code and receive their own persistent, revocable
credential. Installation domains, realm/client, roles and encryption secrets
are configuration, not application constants. The optional
[Keycloak Terraform module](deploy/terraform/keycloak/main.tf) provisions a
client in an existing realm without embedding installation credentials.

`release-control login` enrolls an agent; `release-control connect --profile`
installs a local credential-loading MCP proxy. See [agent connection](docs/AGENT_CONNECT.md).

CI runner labels are supplied by the repository variable `RCP_RUNNER_LABELS`
(JSON array of trusted self-hosted runner labels). Fork PR code is excluded
from these runners. Successful default-branch container builds publish an
`rcp-image-evidence` artifact. An authenticated operator can run
`scripts/deploy-gitops.py` with explicit source repository, run ID, GitOps
repository/branch and manifest. It previews by default; `--apply` verifies
build provenance and publishes the immutable image through Git with a file
SHA concurrency check. It never patches Kubernetes. Credentials come from the
operator's existing GitHub CLI authentication or a scoped CI environment.


Service installation maintenance: [updates, daily checks, backups and rollback](docs/UPDATES.md).

Docker startup, workspace mount and recovery: [DOCKER_LOCAL.md](docs/DOCKER_LOCAL.md).

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

First agent calls: `get_state {}` lists Products; `get_state {"product_id":"…"}` lists that Product's Features; `get_state {"feature_id":"…"}` lists that Feature's Integrations. Then use `get_project_context {"product_id":"…"}` and `resume {"feature_id":"…"}` for the chosen work. Navigation defaults to 25 cards (maximum 100); pass the returned `pagination.next_cursor` with the same scope to continue. It is a compact index, not a backup or readiness report. Fresh databases contain no demo data. Record a structured `handoff` before stopping work.

The default Compose setup is loopback-only, without an API token. PostgreSQL data survives restarts in a named volume. `docker compose --profile app down` stops the services; do not add `-v` unless intentionally deleting data.

For local development, use `make run` (Go 1.25+), optionally setting `RCP_DATA_PATH` and `RCP_WORKSPACE_ROOT`. To use PostgreSQL, run `make db` and supply `DATABASE_URL` explicitly. Do not run both app modes on port 8090 simultaneously.

## Current execution boundary

Implemented: shared Repository Registry, Product/Feature/Integration scopes, unmanaged external dependencies, GitHub App authentication, Git observation, asynchronous Actions/GHCR operations and explicit DEV GitOps changes. The server does not use the local `gh` token. GitHub credentials and workflow/mapping configuration are required.

An opt-in external `cmd/worker` now executes trusted offline source checks and
local OCI builds with dedicated credentials, immutable recipes and per-attempt
container supervision. It does not publish registry images or replace existing
GitHub delivery. See [trusted local worker](docs/LOCAL_WORKER.md) for the exact
security profile, inactive configuration examples and real runtime tests.

A successful GitOps operation records desired state and remains pending reconciliation. Read-only Kubernetes API observation (kubeconfig or in-cluster ServiceAccount), configured Flux observation, managed integration branches and release workflows have separate policies and evidence. GitOps success alone is not runtime health; local tests are not evidence of a successful live deployment. See the delivery and runtime guides for configuration and limits.

## References

- [Agent quickstart](docs/AGENT_QUICKSTART.md) — run, connect, configure, deploy, resume.
- [Complete shared Dev restoration](docs/DEV_RESTORATION.md) — attributed confirmation, external human-only Flux merge, all-original runtime proof and explicit unlock.
- [GitHub build and GitOps contract](docs/GITHUB_DEV.md) — workflow, GHCR and evidence details.
- [External worker protocol](docs/WORKER_EXECUTION.md) and [local execution](docs/LOCAL_WORKER.md).
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

Export/import the complete instance history as gzip through API/MCP or `scripts/backup.mjs`. Restore is atomic and requires an empty destination; ordinary active operations are cancelled, while external worker jobs require explicit reconciliation. Original evidence is retained and no external work is replayed. See [agent backup and migration guide](docs/BACKUP.md).

Codex P1/P2 review comments can become persistent findings through MCP `sync_pull_request_review`. Shared PRs use preview and explicit comment selection. See [PR review workflow](docs/PR_REVIEWS.md).

## Shared testing, independent release

Test selected features together on a versioned environment composition. Release selected ready integrations through main, then build production artifacts from pinned main commits. DEV composition images are not production promotion inputs. See [scenario and release design / implementation status](docs/TEST_AND_RELEASE_FLOW.md).

Frontend verification uses test-only jsdom. Run `npm ci` once before `make check`; the shipped UI remains dependency-free plain JavaScript. See [live UI updates](docs/LIVE_UI.md).

## Published Docker image

GitHub Actions runs checks first, then publishes `ghcr.io/deadnodes/ai-development-workspace`
for `linux/amd64` and `linux/arm64`. Main publishes `latest` and `sha-<full commit>`;
`v*` tags publish the matching version tag. Pull requests run checks without publishing.
The repository and this GHCR package are public. Docker can pull the image without
registry credentials. Pin a version tag or immutable digest for deployments.

```sh
docker run -d --name ai-development-workspace \
  -p 127.0.0.1:8090:8080 \
  -v ai-development-workspace-data:/data \
  ghcr.io/deadnodes/ai-development-workspace:latest
```

This starts embedded persistence. For a workspace mount or PostgreSQL, follow
[the Docker guide](docs/DOCKER_LOCAL.md). Use a version tag or digest to pin deployments.

The separate [local OCI publisher](docs/LOCAL_PUBLISHER.md) publishes an explicitly
approved worker receipt to an exact registry repository and verifies the remote
bytes; RCP artifact admission and deployment remain separate.

The [native publication path](docs/NATIVE_PUBLICATION.md) connects authenticated
worker builds to a separate publisher, independently verifies registry bytes and
admits typed native artifacts for the existing GitOps PR path. Optional operator
bindings let DEPLOY and DEV/TEST COMPOSE queue the same build/publication jobs
automatically, with durable shared-volume archive transport and source fencing.
Native production candidates and main advancement remain blocked; live installation
and the remaining RCP-011 acceptance are separate work.

The [real application fixture](docs/REAL_APPLICATION_BUILDS.md) prepares a pinned
offline RCP toolchain and a verified Git/CA runtime bundle, then exercises the
same native path with actual application source and by-digest runtime checks.

Optional [provider check publication](docs/PROVIDER_CHECKS.md) binds one GitHub
App checks-and-build context to the exact native worker source and accepted
receipt, with durable write reconciliation and managed delivery gates.

The [artifact protection inventory](docs/ARTIFACT_RETENTION.md) reports effective
retention floors, deployment roots, durable manual/rollback pins and admitted
registry-observer coverage from stored evidence. Explicit observer jobs read OCI
objects without registry writes; tag discovery does not prove complete inventory.
Deletion remains disabled. See [registry observer setup](docs/REGISTRY_OBSERVER.md)
for the separate operator recipe, credential and runner configuration.
