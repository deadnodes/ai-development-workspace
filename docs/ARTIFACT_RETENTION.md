# Artifact protection inventory

The retention query is a read-only report from stored RCP evidence. It does not
contact a registry or provider. HTTP `GET /api/products/{product_id}/retention`,
MCP `query` with `get_artifact_retention`, and attention use the same domain
calculation. The browser exposes it under Delivery → Artifact retention.

Every component/profile protects at least the newest 10 known publications.
A saved preference below 10 remains visible, but cannot lower this floor.
Current and previous **complete, proven production releases** remain protected
regardless of saved `keep_current` or `keep_previous` preferences. Additional
configured history can protect more. Production uses the existing binding
purpose `PROD` and the frozen target actually executed, rather than mutable
current environment settings.

Publication ordering needs trusted evidence: the accepted native publication
attempt, or the matching successful frozen GitHub Actions build and its original
artifact observation. Local artifact creation and later refresh times do not
establish publication chronology. Unknown publication times or profiles preserve
images outside the newest-image quota and add blockers. Repeated records of the
same physical image use the earliest trusted publication time.

The inventory combines recorded artifact identities with desired compositions,
frozen operation inputs, current/previous production releases, GitOps expected
and reconciled state, runtime workload/pod evidence, in-flight or uncertain work,
and explicit pins. Old or unhealthy rollout images also need protection. Missing,
stale, tagged, incomplete, or ambiguous observations add blockers rather than
proving that an image is unused. Physical repository/digest sharing with another
Product can add generic protection without exposing foreign entity IDs.

Each canonical `registry/repository@sha256:…` row includes protection reasons,
availability, freshness, and blockers. Registry hosts must be explicit, including
ports where needed; an unqualified GHCR repository is interpreted only where a
frozen GitHub Actions contract proves that origin. Fresh evidence is at most five
minutes old; a zero or future timestamp is unknown. `input_hash` covers normalized
relevant evidence, effective policy and freshness classes. It is stable under
array reordering and clock movement within a freshness class. `evaluated_at`
records the evaluation clock separately.

**`deletion_allowed` is always false.** The hash is neither a deletion capability
nor a compare-and-swap authorization. An “Unprotected recorded” row is advisory,
never permission to delete. OCI closure, complete registry inventory and complete
workload coverage are not established by this report.

## Explicit protection pins

Use ordinary authenticated commands through HTTP `/api/commands` or MCP `execute`.
Create a pin with Product scope and a component belonging to that Product:

```json
{
  "action": "pin_artifact",
  "product_id": "product-id",
  "data": {
    "application_id": "component-id",
    "image_repository": "registry.example:5000/team/image",
    "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "purpose": "rollback",
    "request_key": "rollback-release-42"
  }
}
```

`purpose` is `manual` or `rollback`. Request keys contain 1–128 characters from
letters, digits, `.`, `_`, `:`, `/`, `-`, starting with a letter or digit.
Repositories and digests are canonical immutable identities; no arbitrary notes,
credentials or provider payloads are accepted. Pins need no recorded Artifact
row and make no claim about registry availability.

Within a Product, retrying the same request key and canonical payload returns the
original pin without another audit event. A changed payload is rejected. Retrying
creation after release still returns the original released pin and never
reactivates it. A distinct protection intent needs a new request key.

Release uses the returned pin ID and exact Product/component scope:

```json
{
  "action": "release_artifact_pin",
  "product_id": "product-id",
  "id": "returned-pin-id",
  "data": {"application_id": "component-id"}
}
```

Release is explicit and idempotent. Pins have no expiration, automatic release,
or individual delete operation. Active pins block Product deletion. After all
pins are explicitly released, the existing exact-name-confirmed Product deletion
can cascade released records; append-only create/release audit events survive.

Pin hashes, references and release consistency are validated on store updates,
reads/reopen, and backup restore. A released pin requires exactly one retained
`release_artifact_pin` audit event matching its scope, actor and server release
time; changing only `released_at` cannot remove protection. This is logical
record consistency, not a claim of cryptographic protection against database
tampering. Restore preserves active pins and adds the existing quarantine;
restoration is not garbage-collection authority.

## Verification and remaining work

Local and isolated PostgreSQL fixtures exercise authenticated HTTP/MCP parity,
attention, idempotency, scope rejection, persistence, corruption rejection,
backup/restore, and explicit Product deletion guards without provider I/O. The
browser fixture verifies default 10, saved 3/effective 10, and the rendered report.
These are source acceptance fixtures, not adoption of this version by the live
RCP service or proof of native x86 execution.

Registry inventory refresh, OCI index/child/blob/referrer closure, deletion fences
and pin races, recovery, destructive approval, measured freed disk space and
successful re-pull remain separate stage-04 work. Full shared A+B Dev and selective
Production acceptance also remain. This change performs no registry deletion,
live provider writes, infrastructure change, main/Flux merge, or runtime cutover.
