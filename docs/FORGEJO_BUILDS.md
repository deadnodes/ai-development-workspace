# Forgejo builds (stage 1)

RCP queues configured Forgejo Actions workflows and records OCI provenance. CI
executes repository code and pushes images; RCP reads anonymous registry metadata.
GitHub delivery behavior is unchanged. Multi-registry publication, Forgejo run
cancellation, runner occupancy APIs and production promotion are out of scope.

## Operator configuration

Set `RCP_BUILDS_FILE` to a mounted JSON file at server startup. Empty disables the
build dispatcher. Compose passes this variable to the app; mount the configuration
separately (for example with a Compose override). Configuration is immutable until
restart, rejects unknown fields and validates references, dispatch refs, URL
origins, identifiers and tag templates. No API can enroll build authority.

```json
{
  "connections": [{"id":"forgejo-dev","kind":"forgejo","api_url":"https://dn-dev-git.tail9c31db.ts.net","owner":"deadnodes","token_ref":"forgejo-rcp-bot-token"}],
  "registries": [{"id":"dev-zot","origin":"http://192.168.32.242:5000","insecure_http":true}],
  "pools": [{"id":"build01","max_in_flight":1}],
  "profiles": [{
    "id":"pp-front","product_id":"deadnodes","connection_id":"forgejo-dev",
    "repository":"deadnodes/pp-front","workflow":"build.yml","dispatch_ref":"main","default_branch":"main",
    "pool_id":"build01","registry_id":"dev-zot","image_repository":"deadnodes/pp-front",
    "tag_candidates":["main-{sha8}","br-{sha8}"]
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
{"product_id":"deadnodes","profile_id":"pp-front","request_key":"operator-request-001"}
```

Pass this to MCP `queue_build` or `POST /api/builds`. A Build produces an image
only; it does not deploy or write Flux. Deployment is a separate future capability.
Supply exactly one of `branch` or `sha`, or neither to use the profile default
branch. Branch requests resolve HEAD once. A SHA must be lowercase 40-hex and is
verified with `GET /repos/{o}/{r}/git/commits/{sha}`; any existing commit is allowed,
including commits away from HEAD. Unknown commits are rejected before persistence.

Every service uses `.forgejo/workflows/build.yml` with `workflow_dispatch` only;
push/PR workflows run checks only. Dispatch sends:

```json
{"ref":"main","inputs":{"sha":"<40-hex>","request_id":"<RCP build id>"},"return_run_info":true}
```

`dispatch_ref` selects only the workflow version. The workflow builds exactly
`inputs.sha`, independent of branch movement, and returns `{id,run_number,jobs}`.
Its `run-name: build <sha> <request_id>` is exposed as ActionRun `title` (task
listings use `display_title`). The request ID is the Build ID, at most 64 characters
from `[A-Za-z0-9._-]`. RCP never parses logs, including human-only `RCP_RESULT` lines.

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

The idempotency identity is `(profile_id,source_sha,workflow)` within the
configured Product. Repeating it returns the existing non-failed Build without
another queue audit. FAILED/CANCELLED attempts require an explicit fresh
`request_key`; replay of an old key returns its historical handle. Request keys
are Product-scoped, durable receipts bound to normalized branch/source/profile
and workflow inputs. Rebinding a key rejects `REQUEST_KEY_CONFLICT`.

## Execution and recovery

The context-cancellable server loop runs immediately and every 15 seconds, with
single-flight processing. FIFO admission atomically counts DISPATCHED, RUNNING and
RECONCILIATION_REQUIRED records per configured pool. Before dispatch it checks the
repository's `build.yml` runs. A `workflow_dispatch` run with title
`build <sha> <request_id>` in waiting/running/success attaches with `reused:"run"`.
Push/PR runs never attach. Waiting/running runs block dispatch unless owned by an
active RCP Build already counted in the same pool. Per-pool capacity still applies.
Listings use `workflow_id=build.yml`, without a head SHA filter (the workflow ref
SHA is not the built SHA), and paginate up to 1000 entries. Incomplete observations
defer work rather than treating occupancy as empty.

Before listing runs or dispatching, RCP probes ordered `tag_candidates`. Missing
tags continue to the next candidate; the first tag with matching revision finishes
with `reused:"registry"`. An existing tag with a different revision fails
`ARTIFACT_REVISION_MISMATCH`. Templates allow only `{sha8}` and `{sha}` and must
include a SHA placeholder. The service chooses `main-<sha8>` when the SHA is in
main's history, otherwise `br-<sha8>`, and labels it with the full revision. Existing
tags with the same revision succeed without rebuilding; conflicting revisions fail
without overwriting. The image is `192.168.32.242:5000/deadnodes/<image>:<tag>`.

RCP stores dispatch intent as RECONCILIATION_REQUIRED before network I/O. A lost
response or restart in that window never automatically dispatches again. It
reconciles by workflow + workflow_dispatch event + exact title `build <sha> <our Build id>`. Until evidence appears, uncertainty retains the pool slot.
No automated reset or retry of uncertainty is provided. Operator investigation is
required if Forgejo never accepted the original request. In-flight deadline expiry
also requires reconciliation; terminal run evidence can still close that handle.
Restart resumes durable records. Changed operator authority fails closed rather
than executing old queued intent under a different configuration.

Waiting maps to DISPATCHED; running to RUNNING; failure/skipped/blocked to FAILED;
cancelled to CANCELLED. Successful runs probe the ordered candidate tags from the
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
the same handle. Installing this source or dispatching that workflow requires
an explicit operator request; a local test result does not perform either action.
