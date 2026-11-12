# Forgejo builds (stage 1)

RCP queues configured Forgejo Actions workflows and records OCI provenance. CI
executes repository code and pushes images; RCP reads anonymous registry metadata.
GitHub delivery behavior is unchanged. Multi-registry publication, Forgejo run
cancellation, runner occupancy APIs and production promotion are out of scope.

## Operator configuration

Set `RCP_BUILDS_FILE` to a mounted JSON file at server startup. Empty disables the
build dispatcher. Compose passes this variable to the app; mount the configuration
separately (for example with a Compose override). Configuration is immutable until
restart, rejects unknown fields and validates references, target branches, URL
origins, identifiers and tag templates. No API can enroll build authority.

```json
{
  "connections": [{"id":"forgejo-dev","kind":"forgejo","api_url":"https://dn-dev-git.tail9c31db.ts.net","owner":"deadnodes","token_ref":"forgejo-rcp-bot-token"}],
  "registries": [{"id":"dev-zot","origin":"http://192.168.32.242:5000","insecure_http":true}],
  "pools": [{"id":"build01","max_in_flight":1}],
  "profiles": [{
    "id":"pp-front","product_id":"deadnodes","connection_id":"forgejo-dev",
    "repository":"deadnodes/pp-front","workflow":"dev.yml","default_branch":"main",
    "pool_id":"build01","registry_id":"dev-zot","image_repository":"deadnodes/pp-front",
    "tag_template":"forgejo-{sha8}",
    "targets":{"dev":{"branch":"main"},"image":{"allow_branches":true}}
  }],
  "timeout_seconds":7200
}
```

`api_url` is the HTTPS origin, without `/api/v1`. `token_ref` is a single filename
inside `RCP_SECRET_DIR` (default `/run/secrets`), read for each provider call. Symlink
escapes are rejected. Token contents never enter persisted state or errors.
Registry HTTP requires explicit `insecure_http:true`; HTTPS verifies certificates.
Clients disable redirects, limit responses and enforce 30-second request timeouts.
Tag templates support `{sha8}` and `{sha}`. Pool capacity defaults to 1; build
in-flight timeout defaults to two hours. Capacity is installation-local and counts
RCP Build records, including uncertain dispatches; it cannot reserve a Forgejo
runner against unrelated actors or another RCP installation.

## Agent/API contract

Use `list_build_profiles {"product_id":"deadnodes"}` to discover configured
profiles, then:

```json
{"product_id":"deadnodes","profile_id":"pp-front","target":"dev","request_key":"operator-request-001"}
```

Pass this to MCP `queue_build` or `POST /api/builds`. **dev deploys via the service
repository's own workflow**, including its deploy-dev job; queueing it is an
explicit request for that effect. For an image-only workflow run, use
`target:"image",branch:"feature/example"`. The default branch and configured dev deployment branch
are forbidden for image requests. Operators must select workflows whose other
branches actually satisfy the image-only contract; RCP cannot inspect arbitrary
workflow code to prove absence of side effects.

Optional `sha` must be full lowercase 40-hex and match observed branch HEAD or
`SHA_NOT_HEAD` rejects without creating a Build. HEAD is checked again before
dispatch. Forgejo workflow_dispatch accepts a ref, not a SHA: a branch can still
move between that observation and Forgejo's dispatch. RCP verifies the actual run
SHA and branch, preserves mismatches as `RECONCILIATION_REQUIRED`, and never reports
success for a different source. Protect refs externally if dispatch-time ref
stability is required.

A Build is its own async handle (not an Operation):

| MCP | Authenticated HTTP |
| --- | --- |
| `queue_build` | `POST /api/builds` (JSON request above) |
| `get_build {product_id,build_id}` | `GET /api/builds/{id}?product_id=…` |
| `list_builds {product_id,profile_id?,status?,limit?,cursor?}` | `GET /api/builds?product_id=…&profile_id=…&status=…&limit=25&cursor=…` |
| `cancel_build {product_id,build_id}` | `POST /api/builds/{id}/cancel` with `{product_id}` |
| `list_build_profiles {product_id}` | `GET /api/build-profiles?product_id=…` |

Reads require Product read permission, queue/cancel require Product write permission.
The authenticated principal supplies attribution. Profiles expose no credentials.
List pages default to 25/max100 with filter-bound live ID cursors, not snapshots.
Cancellation accepts only QUEUED builds and does not cancel external runs.

The idempotency identity is `(profile_id,source_sha,workflow,target)` within the
configured Product. Repeating it returns the existing non-failed Build without
another queue audit. FAILED/CANCELLED attempts require an explicit fresh
`request_key`; replay of an old key returns its historical handle. Request keys
are Product-scoped, durable receipts bound to normalized branch/source/target/profile
and workflow inputs. Rebinding a key rejects `REQUEST_KEY_CONFLICT`.

## Execution and recovery

The context-cancellable server loop runs immediately and every 15 seconds, with
single-flight processing. FIFO admission atomically counts DISPATCHED, RUNNING and
RECONCILIATION_REQUIRED records per configured pool. Before dispatch it checks the
workflow's runs: waiting/running work on another SHA makes the queued build wait;
matching branch/SHA waiting/running/success work is attached with `reused:"run"`.
Run listing filters by source SHA or waiting/running status, and is paginated
with a bound of 1000 entries per filter observation; exceeding
that bound defers the build rather than treating incomplete occupancy as empty.

A matching registry revision can finish immediately with `reused:"registry"`.
For dev, this also requires a successful workflow run on the configured branch
and SHA, since image presence alone does not prove the deploy job ran.

RCP stores dispatch intent as RECONCILIATION_REQUIRED before network I/O. A lost
response or restart in that window never automatically dispatches again. It
reconciles by workflow + SHA + workflow_dispatch event + branch, with creation
at/after queued time. Until evidence appears, uncertainty retains the pool slot.
No automated reset or retry of uncertainty is provided. Operator investigation is
required if Forgejo never accepted the original request. In-flight deadline expiry
also requires reconciliation; terminal run evidence can still close that handle.
Restart resumes durable records. Changed operator authority fails closed rather
than executing old queued intent under a different configuration.

Waiting maps to DISPATCHED; running to RUNNING; failure/skipped/blocked to FAILED;
cancelled to CANCELLED. Successful runs resolve the configured tag from the
registry: missing artifacts fail ARTIFACT_MISSING, a different revision label
fails ARTIFACT_REVISION_MISMATCH. Temporary registry read failures defer observation.
Top-level manifest/index digest is hashed and checked against Docker-Content-Digest
when supplied; linux/amd64 manifest and config descriptors are hash/size verified,
and config label `org.opencontainers.image.revision` must equal the full source SHA.
The returned image reference is `origin/repository:tag@sha256:…`, with the index
digest retained when the image includes attestations. This is provenance identity,
not a claim that all layer bytes were downloaded or a signature was verified.

All automated tests use fakes/httptest, never real Forgejo or zot. PostgreSQL tests
use isolated schemas under `TEST_DATABASE_URL`. Live acceptance is separate: on an
explicitly configured installation queue pp-front through MCP, observe its run,
compare returned SHA/digest/revision with zot and confirm duplicate queueing returns
the same handle. Installing this source or running that deploying workflow requires
an explicit operator request; a local test result does not perform either action.
