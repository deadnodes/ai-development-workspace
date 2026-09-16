# Configuration authority: Control Plane → Git mirror

Control Plane is the primary interaction point for the project. Humans use its UI; agents use MCP/API. Both update the same application service and PostgreSQL transaction.

## What lives where

| Data | Authority |
| --- | --- |
| Product, components, attached repositories, provider grants | Control Plane database |
| Build workflows/refs/parameters, deployment mappings and policies | Control Plane database |
| Desired environment composition, external dependencies | Control Plane database |
| Features, integrations, decisions, findings, handoffs, operation history | Control Plane database |
| Source code and source commit history | Source Git repository |
| Published configuration copy | Generated Git mirror; not editable authority |
| Flux input manifests | GitOps repository, generated/updated from the approved deployment plan |
| Image bytes | Registry |
| Secrets | Secret backend / Kubernetes Secret; DB stores references |
| Service bootstrap: DB URL, API token, listener | Deployment configuration / Secrets |

Starting a new container reads existing state from PostgreSQL. It does not reload project configuration from Git, reset it to repository defaults or seed a Product. GitOps and the configuration mirror are different roles: Flux still watches deployable manifests, while the mirror records Control Plane configuration for review.

Configuration commands persist current state and append an attributed audit event atomically. Build/environment configuration versions and operation snapshots preserve earlier settings. History lives in the DB; the audit is not advertised as a fully replayable event-sourcing backup. Back up PostgreSQL, including records and events.

## Current mirror implementation

- HTTP: `GET /api/products/{product_id}/configuration`
- MCP: `get_product_configuration {"product_id":"…"}`
- File export:

```sh
RC_URL=http://127.0.0.1:8090 \
  node scripts/export-configuration.mjs PRODUCT_ID /PATH/TO/PRIVATE-MIRROR/product.json
```

Use `RC_TOKEN` when configured, via the agent's secret environment. The script fetches the persisted state and atomically replaces the destination only after a successful response. It does not read Git as input or commit/push automatically. Commit this file in the mirror repository through your existing authorized Git workflow.

The versioned JSON format includes `authority: control-plane-database`, Product ID, schema version and deterministic SHA-256 content revision. Identical effective configuration produces the same snapshot; Git observation errors and polling do not rewrite it. The revision identifies content, not a monotonically increasing database sequence.

Export includes the selected Product's topology, effective build/deployment/repository bindings, granted connections, attached registered repository identities, related external systems and selected composition snapshots. It excludes unrelated Products, full development/audit history, operation logs and runtime observations. Credential references are included without resolving their values. Metadata, workflow inputs and private repository/interface names may be sensitive: use a private mirror; do not treat this projection as a public sanitized export.

The export is a mirror, not a full backup/import format. A mirror outage or failed Git push does not roll back the authoritative DB update. Retry export after connectivity returns. Editing or deleting the mirror has no effect on the service. Automatic background synchronization, mirror-job status and an explicit validated restore/import flow are not implemented yet; there is no automatic bidirectional merge or startup import.

## Agent rule

Read context from MCP/API → change configuration through commands → observe operation results → record verification and handoff in the service. Export the mirror when needed. Never use the mirror as a competing mutable project state file.

Full application-state backup/restore is now available separately from this configuration mirror: [backup and migration](BACKUP.md). It does not turn mirror JSON into an import format.
