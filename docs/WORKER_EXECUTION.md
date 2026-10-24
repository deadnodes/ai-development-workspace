# External worker protocol and trusted local execution

This protocol extends durable Operations with WorkerJobs and append-only attempts.
It does not execute code inside RCP, publish artifacts or deploy. The separate
[trusted executor](LOCAL_WORKER.md) performs source checks and bounded offline
OCI builds; the server stores typed local-output assertions.
GitHub delivery remains the default existing implementation. External worker
Operations have exactly one lifecycle authority: worker execution service; the
GitHub poller never claims them.

## Frozen state machine and recovery

`QUEUED → CLAIMED → RUNNING → SUCCEEDED | FAILED`.
A claim creates a durable attempt with monotonically increasing fence, authenticated
principal, server expiry and immutable input fingerprint. Start, heartbeat and
report require that exact owner, attempt, fence and fingerprint, before expiry
and before job deadline. The worker cannot supply an actor, lease time or scope.

A CLAIMED lease that expires before start can return to QUEUED, bounded by deadline
and max attempts. A RUNNING lease expiry or cancellation becomes
RECONCILIATION_REQUIRED: never automatically retry uncertain execution. Explicit
operator reconciliation can only close it FAILED/CANCELLED in this nonpublishing
slice; a new approved request is necessary for a new run. Cancellation before
start is terminal CANCELLED. Completion and cancellation serialize atomically;
first terminal transition wins. A terminal identical report replay returns its
original receipt without mutating audit; conflicting replay is rejected. A stale
attempt can never heartbeat, start or report after reclaim. Expiry is checked on
every write, even if no other worker has claimed. Restart never resets fences,
leases, attempts, requests or receipts. A worker must reconcile a lost response by
replaying the identical request, not inventing a new result.

Queue requests require a Product-local `request_key` (16..128 identifier
characters). The original typed request, Integration and attribution are bound
by a durable request fingerprint in the same transaction as the job/operation.
Identical retries return that job without another audit event, including after
deadline expiry or configuration drift. Changed inputs under the same key are
rejected; an intentional new execution requires a new key. Repeating `start`
for the same valid RUNNING attempt returns its receipt without another event;
expired, cancelled or foreign attempts remain rejected.

Backup restoration puts nonterminal external jobs and their latest attempts in
RECONCILIATION_REQUIRED. Their Operations remain nonterminal with the matching
phase and no finished timestamp, and are listed separately in the restore
response's `reconciliation_required_operation_ids`, never as cancelled. Imported
leases are invalidated and no automatic execution is replayed.

Job + attempt + operation + audit transitions use the same Store transaction on
both bbolt and PostgreSQL. Poll/claim also performs bounded expiry recovery.
Failed transaction commits publish no attempt or acknowledgement.

## Authentication and trust

Disabled unless an operator config file is supplied and RC_TOKEN protects the
control-plane API. Worker tokens are independently resolved from environment refs,
never from Actor or global RC_TOKEN. Every request checks expiry, revocation,
Product, repository and capability grants. Worker credentials cannot access
ordinary API, MCP, backup, configuration or admin routes. Operator configuration
owns recipes, immutable revision/hash, repository URL, trust classification and
typed parameter definitions. Requests select an approved recipe and captured
IntegrationRevision; they cannot supply commands, URLs, credential refs or recipe
contents. This slice grants no external write credentials. A receipt is a worker
assertion about a check or verified local OCI output, never proof of registry
availability, independent provenance attestation or rollout.

An optional immutable `execution.source_fetch` profile lets the external
supervisor obtain a pinned GitHub file-tree before its existing offline checks
or build. It binds the numeric repository identity and resource bounds into the
recipe/job hash. Supervisor-only `source_github` configuration chooses explicit
anonymous access or a separate Contents-read token reference; it is never a
job field or a credential forwarded to source execution. Source strategy is
exclusive with `source_mirror`, with no retry using a broader strategy or token.
The reader verifies tree/blob identities while trusting GitHub's commit-to-tree
mapping. It does not grant source writes, execute COMPOSE externally, ingest Git
packs or expose source bytes through RCP. See [source configuration and limits](LOCAL_WORKER.md#github-source-without-a-preseeded-mirror).

Recipes accept only bounded booleans, integers and enumerated strings. No raw
stdout, arbitrary error, URL, prompt, secret or tool payload is persisted. Result
codes are closed; evidence is an optional sha256 fingerprint, not raw output.

## API

Operator commands through existing HTTP/MCP execute: queue_worker_job,
cancel_worker_job, reconcile_worker_job. Generic attribution is not per-user RBAC.
Worker-only POST /api/worker/{claim,start,heartbeat,report} uses dedicated bearer
credentials and strict typed JSON; claim with no eligible job returns null.
`internal/workerclient` supplies the typed isolated client. Configuration loading
is opt-in via RCP_WORKERS_FILE; see configuration structs for the current schema.
The executor profile, bounds and real opt-in tests are documented in
[LOCAL_WORKER.md](LOCAL_WORKER.md). Fleet deployment, registry credentials,
external publication fencing and existing delivery/build routing remain pending.
