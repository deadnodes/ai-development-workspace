# Direct model tasks and customer release notes

RCP can call an operator-enrolled OpenAI-compatible provider directly, without
starting Codex, Claude Code or another agent session. Typed tasks use the existing
durable Operations scheduler. The feature is disabled without configuration.
Generated text is review-required material, separate from operational logs,
verification evidence and release approval.

## Operator configuration

`RCP_INTELLIGENCE_FILE` names an operator-owned JSON file loaded at server startup.
Example with a placeholder endpoint and model:

```json
{
  "base_url": "https://models.example.invalid/v1",
  "model": "configured-model",
  "api_key_env": "RCP_MODEL_API_KEY",
  "encryption_key_env": "RCP_MODEL_ENCRYPTION_KEY",
  "product_ids": ["example-product"],
  "conflict_paths": {
    "example-repository-id": ["internal/example/service.go"]
  },
  "allow_private_endpoint": false,
  "max_input_bytes": 32768,
  "max_output_bytes": 32768,
  "max_completion_tokens": 4096,
  "max_requests_per_day": 50,
  "timeout_seconds": 30
}
```

The encryption-key reference must resolve to a base64-encoded 32-byte key. Keep
both keys outside Git, ordinary RCP records and reports. Product grants and
conflict file paths are explicit; neither grants merge or deployment authority.
Normal principal and production-reference restrictions still apply: a
production-scoped draft request can require an administrator even though it
does not deploy anything. Model enrollment grants no additional user role.
Task dispatch also requires the existing scheduler: `RCP_WORKER_ENABLED=false`
leaves tasks queued. Enabling that scheduler can resume other pending operations,
so check them during the operator's normal idle-operation preflight.
Invalid configuration fails closed. Configuration or key changes can invalidate
pending tasks and old drafts. Retain the correct encryption key for history that
must remain readable. Enrollment and shared-service changes are separate
operator actions under [UPDATES.md](UPDATES.md), not a consequence of source CI.

The adapter sends HTTPS `POST {base_url}/chat/completions`, non-streaming JSON
output and `max_completion_tokens`, following the [official API contract](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create).
A compatible provider must support these fields; limits are never silently
dropped. Redirects and environment HTTP proxies are disabled. DNS addresses are
validated and the selected address is pinned per dial. Private-address access
requires explicit configuration; TLS validation remains enabled.

The request limit applies per Product over a rolling 24-hour window. Attempts
are counted conservatively, including uncertain dispatches. Input/output byte
bounds, completion-token limits and HTTP timeouts also apply. Missing or partial
provider usage is unknown, not reported zero. Monetary cost remains unknown;
these controls are not a guaranteed currency spending cap.

## Queue, inspect and cancel

Supported `task_kind` values are `annotation`, `summary`, `log_summary`,
`release_notes` and `conflict_patch`. Use ordinary MCP `execute` or
`POST /api/commands`, for example:

```json
{
  "action": "queue_intelligence_task",
  "actor": "agent/example",
  "product_id": "example-product",
  "feature_id": "example-feature-id",
  "data": {
    "task_kind": "summary",
    "request_key": "feature-summary-example-0001"
  }
}
```

Inspect the returned operation through MCP `get_intelligence_task` (`task_id`) or
`GET /api/intelligence-tasks/{id}`. The query separates generated text from
internal provenance and reports stale inputs. Ordinary state/export records
contain authenticated ciphertext and minimal metadata. Result queries remain
subject to normal authentication and task/Product authority.

Repeating an identical `request_key` returns the existing task; different inputs
under that key are rejected. Cancel using `cancel_intelligence_task` with the
operation `id`, `product_id` and an empty `data` object. Cancellation fences late
results, but cannot undo work or charges already incurred at the provider.

Dispatch intent is persisted before I/O. An uncertain request or restart after
dispatch intent does not automatically replay a potentially paid call. Inspect
the blocked/failed task before explicitly requesting new work. Application
idempotency does not imply exactly-once provider billing.

Raw input and output use authenticated application-layer encryption. Audit
records contain safe identities, status and fingerprints. Supply only minimal
approved context: the conservative privacy filter can reject obvious sensitive
content but is not proof that arbitrary logs or files contain no sensitive data.
Model output is untrusted data, never a command or permission.

## Release preparation

Queue `release_notes` with `candidate_operation_id` and either one
`previous_release_id` or `previous_release_ids` keyed by application ID. These
baseline forms are mutually exclusive. Feature/Integration membership is derived
from the Product's release evidence, not a caller-supplied list of ready work.

The frozen candidate must have exact source/artifact provenance. Recorded
released baselines must match the same environment's current healthy runtime
observations, including source, digest and GitOps identities. Observations must
be fresh. Per-component baselines support different previous release versions.
Missing or ambiguous evidence is actionable unknown state, not an assumed empty
first release or an inferred deployment based on release creation order.

Compare captured Integration revisions: advancing a repository SHA does not make
every Integration in it new. Already released unchanged work is omitted; a new
revision of released work can be included. Candidate preparation captures an
intent fingerprint. Changed intent or an older candidate without the capture
requires fresh preparation, rather than presenting future work as shipped.

Customer prose describes supported benefits, fixes, limitations and necessary
actions; internal Integration/evidence references stay separate. Candidate,
baseline or context changes invalidate the draft. Prepare and review it before
merging the release Flux PR. The existing operations UI has an explicit preview
action that reads the draft without generating or publishing it. Customer
mailing, publication channels and a mandatory editorial merge gate are separate
decisions.

## Small-conflict assistance

`conflict_patch` binds an existing conflict, exact source inputs and bounded
file preimages. A response is only a proposal. A `source_check` worker job can
reference it with `intelligence_task_id`; normal trusted recipe, source revision,
authority and deadline requirements still apply. Only the authenticated lease
response carries the decrypted proposal. Persisted worker jobs and local journals
retain an immutable binding rather than plaintext replacement source.

An external worker applies the proposal in its exclusively owned materialized
snapshot before running trusted recipe checks. The helper permits at most three
existing text files and 32 KiB of combined preimage/replacement text. It validates
base/head SHAs, exact content hashes and paths; rejects symlinks, unresolved Git
conflict markers and sensitive/configuration/dependency/migration paths; and
preserves file modes. All files are preflighted before writes. Rollback is guarded
and best effort; multi-file replacement is not advertised as crash-atomic.

Check evidence is bound to the patch fingerprint. Modified source cannot enter
the ordinary OCI publication path under the original clean source SHA. Passing
checks does not resolve the conflict, push a resolution commit, merge or deploy.
Existing resolution-commit admission, independent review and release approvals
remain necessary. Failure, staleness, oversize or ambiguity leaves the
requesting-agent fallback available.

## Verification boundary

Adapter wire tests use isolated fake TLS endpoints. Application/transport tests
substitute only the completion port while retaining real task state, crypto and
authority. Browser fixtures use synthetic API responses. They establish source
behavior, not real provider access, model quality, billing acceptance or an
installed-service rollout.
