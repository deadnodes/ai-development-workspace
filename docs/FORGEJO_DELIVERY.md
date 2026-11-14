# Finished Forgejo builds → Dev GitHub Flux PR

`deploy_build` is a separate, explicit delivery request for a finished Build.
RCP verifies the source CI and immutable artifacts, proposes bounded GitOps image
changes on GitHub and squash-merges its own PR with a server-side head SHA lock.
Merging an application repository deploys nothing. Production is rejected by
configuration and API. SUCCEEDED confirms runtime rollout when verification is configured; otherwise `rollout_verified: false` records GitOps completion only.

## Operator authority

Add to `RCP_BUILDS_FILE` (full example: [forgejo-builds.json](../examples/forgejo-builds.json)):

```json
{
  "gitops": [{
    "id": "github-flux", "kind": "github", "api_url": "https://api.github.com",
    "app_id": 4967151, "installation_id": 162235198,
    "private_key_ref": "github-app.pem", "repository": "deadnodes/flux", "branch": "main"
  }],
  "environments": [{"id": "dev", "gitops_id": "github-flux", "allow_auto_merge": true}]
}
```

Each build profile may add:

```json
{
  "required_status_contexts": ["pp-front dev / verify (push)"],
  "deployments": {"dev": {"files": [{
    "path": "clusters/platform-dev/custom-resources/lp-dev/pp-front.yaml",
    "image_id": "web", "image_repository": "192.168.32.242:5000/deadnodes/pp-front",
    "tag_format": "{tag}@{digest}", "min_matches": 1
  }]}}
}
```

All objects reject unknown fields. Only Dev with auto merge enabled and base
branch main are accepted. Targets must reference a configured profile image and
its registry repository. The file `format` defaults to `helm_values`; its tag format is `{tag}@{digest}` or `{digest}`.
The other supported format is `container_image`; unknown formats are rejected.
For `container_image`, `tag_format` is ignored and may be omitted. Each
file selector requires a positive min_matches (maximum 100). Up to 16 entries
per profile are supported; several image selectors may share a file.
The example paths are operator templates: verify them against the installation.

The PEM is a single filename beneath `RCP_SECRET_DIR` (default `/run/secrets`),
with symlink confinement and bounded reads. The existing GitHub App JWT → cached
installation-token provider handles both stored and explicit credentials. The
startup file is sufficient authority even with an empty database; no State
GitHub connection is required. Give the App Contents read/write and Pull requests
read/write on Flux. Branch protection may reject merging; no bypass is attempted.
Custom HTTPS API origins in this operator file are explicit destination authority.
No API can modify this configuration. Restart is required for changes.

## API and state

| MCP | HTTP |
| --- | --- |
| `close_deployment {product_id,deployment_id,reason,request_key?}` | `POST /api/deployments/{id}/close` with `{product_id,reason,request_key?}` |
| `deploy_build {product_id,build_id,environment_id:"dev",request_key?,allow_rollback?,reason?}` | `POST /api/deployments` with the same JSON |
| `get_deployment {product_id,deployment_id}` | `GET /api/deployments/{id}?product_id=…` |
| `list_deployments {product_id,environment_id?,profile_id?,limit?,cursor?}` | `GET /api/deployments?product_id=…` with the same query fields |

Creation requires Product write permission; reads require Product read permission.
List defaults to 25, maximum 100; cursors bind Product/environment/profile filters.
The authenticated principal supplies attribution. Records and request receipts
persist in bbolt and PostgreSQL through the shared State document codec.

`PENDING_CHECKS → PROPOSING → MERGING → VERIFYING_ROLLOUT (when configured) → SUCCEEDED | FAILED` runs in the existing
15-second cancellable dispatcher. CI pending or missing required contexts polls
until the configured build timeout deadline. Required context failure/error or
pending expiry fails CI_NOT_GREEN. With no required contexts, combined state must
be success. Incomplete/unavailable provider evidence defers execution. Every
configured build image requires a fresh HEAD of its exact registry digest;
404 fails ARTIFACT_MISSING. CI and registry timestamps/evidence are persisted.

The renderer parses multi-document Flux HelmRelease YAML to locate matching
`spec.values.image.repository`, then edits only that mapping's `tag` scalar.
It preserves quote style, indentation, comments and all other bytes, then reparses
to check semantic scope. A selector below min_matches fails GITOPS_TARGET_MISSING.
Anchored, multiline, ambiguous or unsupported targets fail closed. All exact
values already present short-circuit GitOps with already_applied (then verify rollout when configured) and no PR.

For plain Kubernetes manifests, use an entry such as:

```json
{
  "path": "clusters/platform-dev/custom-resources/meteroid-dev/workloads.yaml",
  "format": "container_image",
  "image_id": "web",
  "image_repository": "192.168.32.242:5000/deadnodes/meteroid-web",
  "min_matches": 1
}
```

The same format applies to plain manifests such as
`clusters/platform-dev/custom-resources/lp-dev/pp-content-cms.yaml` with that
profile's configured image ID and repository. Across all YAML documents, each
`containers` or `initContainers` entry with an exact matching image repository
is rewritten to `<image_repository>:<tag>@<digest>`. Registry ports are preserved
when removing the previous tag/digest; repositories sharing only a prefix do not
match. Only the image scalar token changes, preserving quotes, indentation,
comments and every other byte. The same min_matches, already-applied short-circuit
and reparse verification apply. Anchored, multiline or flow targets fail closed.

RCP creates `rcp/deploy/<deployment-id>` and a PR containing Build ID, source SHA,
run URL, image digests and CI evidence. It rereads the PR before merging, checks
mergeability, main base, unchanged head/repository and the complete Git tree diff,
and verifies exact rendered bytes. Merge uses `squash` and `sha:<PR head>`.
HTTP 405/409 causes at most one re-render against fresh main, on the same branch
with an expected-head check and non-force fast-forward update. Persistent conflict
fails MERGE_CONFLICT and leaves the PR open. Runtime observers are optional.

Only active (non-terminal) deployments deduplicate by Product/environment/profile
and the sorted image digest set. Without a request key, or with a new key, an
identity matching only terminal deployments creates a new Deployment. Rendering
returns already_applied without a PR (then verifies rollout when configured) if Flux already has the requested
values; otherwise it opens a PR, including when rolling back to a previous digest.
Product-scoped request keys are durable receipts bound to Build and environment.
Replay returns the original historical handle, including terminal deployments;
rebinding rejects REQUEST_KEY_CONFLICT.

## Recovery and verification

Intent is persisted before provider mutation. Lost proposal/merge responses become
RECONCILIATION_REQUIRED. Reconciliation looks up the exact branch and PR; an
uncertain merge only reads until merged evidence appears, and never re-merges.
When a proposal never reached PR creation (or no unique PR is observable), the
handle stays uncertain for operator investigation. Do not blindly create a new
request. Uncertain handles retain exclusive ownership of overlapping Dev files.
Normal process restart resumes stored phases; backup restore quarantines unfinished
deployments. Authority changes prevent old intents from executing and require
operator reconciliation. No timeout silently releases uncertain file ownership.

Offline tests use in-memory transports and fake adapters, never real GitHub,
Forgejo or zot. PostgreSQL persistence tests require TEST_DATABASE_URL and an
isolated schema. Live installation acceptance remains separate and was not
performed for this change.

## Closing stuck builds

Instance admins can use MCP `close_build {product_id, build_id, reason, request_key?}`
or `POST /api/builds/{id}/close` with JSON `{product_id, reason, request_key?}`.
Requires instance admin authorization, not Product write permission.
Only DISPATCHED, RUNNING and RECONCILIATION_REQUIRED handles may be closed.
Use `cancel_build` for QUEUED builds. Terminal handles reject closure except
for an identical request-key replay.

The reason is trimmed, must contain 8..500 characters, and is stored without
further rewriting. Closure records FAILED / OPERATOR_CLOSED and a finish time,
preserves all provider and artifact evidence, and journals the operator, reason,
previous status/failure code and provider run ID. It never accepts artifacts
or re-dispatches the handle and immediately frees RCP pool admission.

**RCP does not cancel the Forgejo run.** Operators must cancel active runs in
Forgejo. Provider-side active-run safety gates still apply to subsequent builds.
Replay with the same optional request key and reason returns the closed build
without another journal event.

## Rollback and concurrency guards

Request-key replay returns the stored handle for the same build/environment even
if `allow_rollback` or `reason` changes. Active deployments with the same product,
environment, profile and image identity still deduplicate. A different identity
in the same environment with the same profile or overlapping GitOps target files
is refused with `DEPLOYMENT_IN_PROGRESS:` naming its deployment, build and status.
Wait for that handle with `get_deployment`; there is no concurrency override.

Before creating a deployment, RCP selects the latest SUCCEEDED Dev deployment for
that product/profile by FinishedAt (UpdatedAt fallback), including `already_applied`.
Equal source SHAs need no ancestry check. Otherwise it checks whether the requested
build is an ancestor of that deployed SHA using the source profile connection and
repository. This uses the commits API `not` filter (Forgejo/Gitea ≥ 1.22) and does
not need the pull-request unit. Newer or diverged commits are allowed.
`ROLLBACK_REFUSED:` blocks older commits; `ROLLBACK_CHECK_UNAVAILABLE:` fails closed
when ancestry cannot be established. The bounded 15-second check runs outside the
store transaction; a changed current deployment returns
`DEPLOYMENT_STATE_CHANGED: retry deploy_build` for a fresh explicit attempt.

Only with owner/coordinator rollback intent, pass `allow_rollback:true` and a
`reason` of 8–500 trimmed runes to override an ancestor or unavailable check.
A reason without the flag is invalid. An override actually used records
`rollback_from_sha`, `rollback_from_deployment_id` and `override_reason` on the
Deployment, and `allow_rollback`, `reason`, both rollback-from fields and
`rollback_check` (`ancestor` or `unavailable`) in the `deploy_build` event.
An unused override records none of these fields.

Limitation: “current” is RCP deployment history for that profile only; manual Flux
edits are not seen. GitOps completion does not prove runtime or functional health.

## Closing a deployment

Instance admins may use `close_deployment` to release `DEPLOYMENT_IN_PROGRESS`
held by a deployment in `PENDING_CHECKS` or `VERIFYING_ROLLOUT`. Other active states are
refused because a GitHub proposal or merge may be in flight. Rollout verification has no pending GitOps writes; closing it does not undo the merge. The trimmed reason must contain 8–500 runes.
Closure records `FAILED` / `OPERATOR_CLOSED`, `close_reason`, timestamps and an
append-only audit event. A matching request key replays without another event;
reusing the key for another deployment or reason returns `REQUEST_KEY_CONFLICT`.

### Authority scope

Build authority is scoped to each profile (excluding deployments and required status contexts), its connection,
pool and registry, and global build timeout and run lookback. Deployment authority
also includes that profile's required status contexts, dev deployment files and
the dev GitOps authority and, for profiles with rollout targets, rollout configuration.
Changes to unrelated profiles do not invalidate builds or deployments. A temporary
legacy shim accepts the former global hash only while the complete configuration
is unchanged. A restart is required for config changes.

## Rollout verification

Configure the optional top-level rollout verifier and a target on each deployment file:

```json
{
  "rollout": {"in_cluster": true, "timeout_seconds": 900},
  "profiles": [{
    "deployments": {"dev": {"files": [{
      "path": "clusters/dev/platform-control-api.yaml",
      "image_id": "runtime",
      "image_repository": "192.168.32.242:5000/deadnodes/platform-control-api/runtime",
      "tag_format": "{tag}@{digest}",
      "min_matches": 1,
      "rollout": {"namespace": "lp-dev", "helm_releases": ["platform-control-api", "platform-control-api-command-consumer"]}
    }]}}
  }]
}
```

This fragment extends the existing profile configuration. For raw container-image manifests,
use `"format": "container_image"` and `"rollout": {"namespace": "meteroid-dev", "deployments": ["meteroid-api"]}`.
Exactly one of `helm_releases` or `deployments` is required, with 1..20 unique
DNS-1123 workload names. Namespaces must also be DNS-1123 labels. Singular fields are unsupported.
Timeout defaults to 900 seconds and accepts 60..3600 seconds.

After merge or `already_applied`, `VERIFYING_ROLLOUT` holds the deployment concurrency guard.
`merged_at` and `rollout_deadline` preserve the verification window. Rollout observations run with concurrency bounded to four deployments and one shared 25-second budget per dispatcher pass, capped by each deployment’s remaining rollout window. Targets within a deployment run with concurrency bounded to four and a 10-second timeout per target. Other deployment work runs before rollout observations. Each dispatcher tick
performs read-only Kubernetes checks outside the store lock. HelmRelease values must contain
the rendered reference and both its observed generation and lastAttemptedGeneration must equal its generation before failures
or readiness count; each condition must also have observedGeneration at least equal to metadata.generation. Ready=False with UpgradeFailed, InstallFailed, RetriesExceeded,
ArtifactFailed or ReconciliationFailed, or Stalled=True, fails verification. Ready=True
also requires, independently for each listed HelmRelease, at least one Helm-labelled Deployment matching the configured image repository.
Every matching container must use the expected digest, and every matching Deployment must
have observed its generation with total, updated, ready and available replicas equal to desired replicas and no unavailable replicas
(zero replicas counts as complete). Raw Deployment targets use the same digest and replica checks;
Progressing=False / ProgressDeadlineExceeded fails immediately.

READY becomes SUCCEEDED with `rollout_verified: true`. Failure becomes FAILED /
ROLLOUT_FAILED with `rollout_failure` (a controller reason token and a redacted message of up to 300 runes). `rollout_failure` stores the HelmRelease/Deployment condition reason and message after pattern-based credential redaction, visible to RCP readers of the product; this is the same text Kubernetes shows to anyone who can read the HelmRelease. Kubernetes GET errors remain pending;
deadline expiry becomes FAILED / ROLLOUT_TIMEOUT with the last reason. Failed rollouts do
not replace the latest SUCCEEDED deployment used by rollback checks. Fix forward with a new
build after ROLLOUT_FAILED; do not redeploy the same build expecting a different result.

The in-cluster ServiceAccount needs `get` and `list` on `helmreleases` in API group
`helm.toolkit.fluxcd.io`, plus `get`/`list` on `apps` deployments, in every configured
namespace. Namespaces may differ per deployment file.
Apply this RBAC separately through Flux. No Kubernetes mutations are made.
Without rollout targets or a verifier, success retains GitOps-only semantics and
`rollout_verified: false`; targets without a verifier produce one startup WARN.
This is point-in-time controller/spec rollout evidence, not pod imageID inspection,
functional acceptance, automatic remediation or production approval. Only configured files
are verified; unconfigured files retain GitOps-only coverage.
