# Finished Forgejo builds → Dev GitHub Flux PR

`deploy_build` is a separate, explicit delivery request for a finished Build.
RCP verifies the source CI and immutable artifacts, proposes bounded GitOps image
changes on GitHub and squash-merges its own PR with a server-side head SHA lock.
Merging an application repository deploys nothing. Production is rejected by
configuration and API. SUCCEEDED records GitOps completion, never runtime health.

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
its registry repository. The tag format is `{tag}@{digest}` or `{digest}`. Each
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
| `deploy_build {product_id,build_id,environment_id:"dev",request_key?}` | `POST /api/deployments` with the same JSON |
| `get_deployment {product_id,deployment_id}` | `GET /api/deployments/{id}?product_id=…` |
| `list_deployments {product_id,environment_id?,profile_id?,limit?,cursor?}` | `GET /api/deployments?product_id=…` with the same query fields |

Creation requires Product write permission; reads require Product read permission.
List defaults to 25, maximum 100; cursors bind Product/environment/profile filters.
The authenticated principal supplies attribution. Records and request receipts
persist in bbolt and PostgreSQL through the shared State document codec.

`PENDING_CHECKS → PROPOSING → MERGING → SUCCEEDED | FAILED` runs in the existing
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
values already present short-circuit with SUCCEEDED / already_applied and no PR.

RCP creates `rcp/deploy/<deployment-id>` and a PR containing Build ID, source SHA,
run URL, image digests and CI evidence. It rereads the PR before merging, checks
mergeability, main base, unchanged head/repository and the complete Git tree diff,
and verifies exact rendered bytes. Merge uses `squash` and `sha:<PR head>`.
HTTP 405/409 causes at most one re-render against fresh main, on the same branch
with an expected-head check and non-force fast-forward update. Persistent conflict
fails MERGE_CONFLICT and leaves the PR open. Runtime observers are optional.

Non-failed deployments deduplicate by Product/environment/profile and the sorted
image digest set. Product-scoped request keys are durable receipts bound to Build
and environment. Replay returns the original handle; rebinding rejects
REQUEST_KEY_CONFLICT. Failed retry requires a new explicit key.

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
