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
  "connections": [
    {
      "id": "forgejo-dev",
      "kind": "forgejo",
      "api_url": "https://dn-dev-git.tail9c31db.ts.net",
      "owner": "deadnodes",
      "token_ref": "forgejo-rcp-bot-token"
    }
  ],
  "registries": [
    {
      "id": "dev-zot",
      "origin": "http://192.168.32.242:5000",
      "insecure_http": true
    }
  ],
  "pools": [
    {
      "id": "build01",
      "max_in_flight": 1
    }
  ],
  "profiles": [
    {
      "id": "pp-front",
      "product_id": "deadnodes",
      "connection_id": "forgejo-dev",
      "repository": "deadnodes/pp-front",
      "workflow": "build.yml",
      "dispatch_ref": "main",
      "default_branch": "main",
      "pool_id": "build01",
      "registry_id": "dev-zot",
      "tag_candidates": [
        "main-{sha8}",
        "br-{sha8}"
      ],
      "images": [
        {
          "id": "web",
          "repository": "deadnodes/pp-front"
        }
      ]
    },
    {
      "id": "pp-back",
      "product_id": "deadnodes",
      "connection_id": "forgejo-dev",
      "repository": "deadnodes/pp-back",
      "workflow": "build.yml",
      "dispatch_ref": "main",
      "default_branch": "main",
      "pool_id": "build01",
      "registry_id": "dev-zot",
      "tag_candidates": [
        "main-{sha8}",
        "br-{sha8}"
      ],
      "images": [
        {
          "id": "runtime",
          "repository": "deadnodes/pp-back/runtime"
        }
      ]
    },
    {
      "id": "meteroid",
      "product_id": "deadnodes",
      "connection_id": "forgejo-dev",
      "repository": "deadnodes/meteroid",
      "workflow": "build.yml",
      "dispatch_ref": "main",
      "default_branch": "main",
      "pool_id": "build01",
      "registry_id": "dev-zot",
      "tag_candidates": [
        "main-{sha8}",
        "br-{sha8}"
      ],
      "images": [
        {
          "id": "api",
          "repository": "deadnodes/meteroid-api"
        },
        {
          "id": "scheduler",
          "repository": "deadnodes/meteroid-scheduler"
        },
        {
          "id": "web",
          "repository": "deadnodes/meteroid-web"
        }
      ]
    }
  ],
  "timeout_seconds": 7200
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

Pass this to MCP `queue_build` or `POST /api/builds`. A Build produces its configured images
only; it does not deploy or write Flux. Deployment uses a separate explicit [`deploy_build` request](FORGEJO_DELIVERY.md).
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
Forgejo 15.0.11 ignores `run-name`; ActionRun `title` is the workflow name.
The effective event is `event` when non-empty, otherwise `trigger_event` (each
limited to 64 bytes). Forgejo 15.0.11 returns an empty `event` and carries
`workflow_dispatch` in `trigger_event`; neither field means no match.
Run identity comes from `event_payload`, a JSON string in run list/detail responses,
whose `inputs.sha` is the built SHA and `inputs.request_id` is the Build ID.
RCP parses at most 256 KiB per run, skips larger or malformed payloads and tolerates
absent fields. It never logs event payloads. The request ID is at most 64 characters
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
and workflow inputs. Rebinding a key rejects `REQUEST_KEY_CONFLICT`. Automatic deduplication requires
the current authority hash; old receipt replay still returns its historical Build
after registry, repository or image configuration changes.

## Execution and recovery

The context-cancellable server loop runs immediately and every 15 seconds, with
single-flight processing. FIFO admission atomically counts DISPATCHED, RUNNING and
RECONCILIATION_REQUIRED records per configured pool. Before dispatch it checks the
repository's `build.yml` runs. A `workflow_dispatch` run with `event_payload.inputs.sha == source_sha`
(any request ID) in waiting/running/success attaches with `reused:"run"`.
Push/PR runs never attach. Waiting/running runs block dispatch unless owned by an
active RCP Build already counted in the same pool. Per-pool capacity still applies.
All run listings use `workflow_id=build.yml` and `event=workflow_dispatch`.
The active-run gate queries `status=waiting` and `status=running` separately.
Reuse and uncertain-dispatch reconciliation do not filter by `head_sha`: it is
the workflow ref commit, which can differ from the built source. They verify
dispatch inputs locally and scan newest-first, stopping at the time window.
Reconciliation starts at the Build queued time minus two minutes of clock skew;
reuse looks back `run_lookback_seconds` (default 86400, range 1–31536000).
Each query is capped at ten pages of 50 runs. Hitting that cap before reaching
the window end is reported explicitly in `last_error`; admission never requires
the complete workflow history. Listing errors
or exhausted bounds set `provider_unavailable:true` and a redacted `last_error`
on the Build, preserving QUEUED or the uncertain state and its pool slot.
A successful listing clears these transient fields. Incomplete observations
defer work rather than treating occupancy as empty.

Before listing runs or dispatching, RCP probes ordered `tag_candidates`. Missing
tags continue to the next candidate; the first tag with ALL configured images present
and matching revision finishes
with `reused:"registry"`. Partial presence dispatches normally; the workflow skips
existing images. Images cannot be combined across different tags. An existing tag with a different revision fails
`ARTIFACT_REVISION_MISMATCH`. Templates allow only `{sha8}` and `{sha}` and must
include a SHA placeholder. The service chooses `main-<sha8>` when the SHA is in
main's history, otherwise `br-<sha8>`, and labels it with the full revision. Existing
tags with the same revision succeed without rebuilding; conflicting revisions fail
without overwriting. The image is `192.168.32.242:5000/deadnodes/<image>:<tag>`.

RCP stores dispatch intent as RECONCILIATION_REQUIRED before network I/O. A lost
response or restart in that window never automatically dispatches again. It
reconciles by workflow + workflow_dispatch event + `event_payload.inputs.sha == source_sha` and
`event_payload.inputs.request_id == our Build ID`. Until evidence appears, uncertainty retains the pool slot.
No automatic redispatch occurs. A RECONCILIATION_REQUIRED Build with a non-null
`dispatched_at` strictly older than configured `timeout_seconds` (default 7200)
plus a fixed 600-second grace can become FAILED / RECONCILIATION_TIMEOUT, releasing
its slot. RCP first re-reads a known `provider_run_id`; matching terminal evidence
still closes the handle normally and matching active evidence retains it. It then
queries waiting and running runs separately, with no age cutoff, for the configured
workflow and effective workflow_dispatch event. Any such run whose `inputs.sha`
equals the Build source OR whose ID equals `provider_run_id` prevents expiry,
regardless of request ID. Both queries must complete within the ten-page bounds;
provider errors (including failed GetRun) or exhausted bounds retain uncertainty
and the slot. Missing dispatch time, quarantined restore records and changed
operator authority do not qualify for this escape. Source-mismatch records with
a known run ID self-heal on polling when fresh run metadata matches.

`get_attention_required` includes up to 100 reconciliation Build entries per
Product, ordered by ID, with `id`, `profile_id`, `failure_code`, and `age_seconds`
since dispatch (queue time when dispatch time is absent).
Restart resumes durable records. Changed operator authority fails closed rather
than executing old queued intent under a different configuration.

Waiting maps to DISPATCHED; running to RUNNING; failure/skipped/blocked to FAILED;
cancelled to CANCELLED. Successful runs probe the ordered candidate tags from the
registry: missing artifacts fail ARTIFACT_MISSING, a different revision label
fails ARTIFACT_REVISION_MISMATCH. Temporary registry read failures defer observation.
Top-level manifest/index digest is hashed and checked against Docker-Content-Digest
when supplied; linux/amd64 manifest and config descriptors are hash/size verified,
and config label `org.opencontainers.image.revision` must equal the full source SHA.
Success requires every configured image at the same chosen tag. Build `images`
is a list of `{id,image_ref,digest}`, preserving each image identity.
Each returned image reference is `origin/repository:tag@sha256:…`, with the index
digest retained when the image includes attestations. This is provenance identity,
not a claim that all layer bytes were downloaded or a signature was verified.

All automated tests use fakes/httptest, never real Forgejo or zot. PostgreSQL tests
use isolated schemas under `TEST_DATABASE_URL`. Live acceptance is separate: on an
explicitly configured installation queue pp-front through MCP, observe its run,
compare returned SHA/digest/revision with zot and confirm duplicate queueing returns
the same handle. Installing this source or dispatching that workflow requires
an explicit operator request; a local test result does not perform either action.

## Live observations and stage 2

Verified by the operator on Forgejo 15.0.11: pp-front source `a72f4b92…`
produced `main-a72f4b92` with the revision label equal to the full source SHA.
Repeating dispatch with the same SHA skipped the build and returned the same digest.
`return_run_info:true` returned `{"id":…, "run_number":…, "jobs":["build"]}`.
Meteroid builds api, scheduler and web in one dispatch, each using the same
`main-<sha8>` or `br-<sha8>` tag and full revision label.
These are supplied live facts; this change's validation performs no live requests.

The configuration supports `required_status_contexts` and Dev targets; see [Forgejo delivery](FORGEJO_DELIVERY.md). Required contexts:

| Profile | Required status context |
| --- | --- |
| pp-front | `pp-front dev / verify (push)` |
| pp-back | `pp-back baseline / gate (push)` |
| meteroid | `meteroid ci / gate (push)` |

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
