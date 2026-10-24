# Capture shared Dev before a candidate

`capture_candidate_dev_baseline` records the physical baseline that a future
restoration must return to. It runs after reservation and before any candidate
activation. A reservation's saved desired composition alone cannot prove what
was running. A capture made after candidate effects cannot reconstruct history.

The capture covers every component in the reserved shared composition, including
components untouched by the candidate. Each needs complete operator-configured
runtime coverage as described in [runtime verification](DEV_RUNTIME_VERIFICATION.md).
Missing roles, ambiguous image selectors or unavailable registry authority block
capture; the caller cannot provide substitute images or observations.

## Request

Named HTTP: `POST /api/products/{id}/candidate-dev/capture-baseline`. Named MCP
and generic `execute` use the same command:

```json
{
  "action": "capture_candidate_dev_baseline",
  "actor": "agent/developer",
  "product_id": "your-product",
  "data": {
    "reservation_operation_id": "your-reservation-receipt",
    "request_key": "baseline-request-0001",
    "expected_selection": {
      "composition_id": "shared-baseline",
      "operation_id": "baseline-operation",
      "generation": 42
    }
  }
}
```

The same attributed owner must still hold the active, nonquarantined reservation
at the exact selection and epoch. Attribution does not establish human approval.
Do not send digests, targets, registry credentials, `healthy`, approval flags or
raw manifests. The server derives its scope from existing configuration.

Exact request-key replay returns the historical receipt without new reads,
audit events or execution rights. A new request creates a new capture generation.
A pending, failed or damaged latest capture cannot reuse an earlier PASS.

## Evidence and registry reads

The server pins a common GitOps head for each physical API and numeric
repository/ref, selects each component using its frozen current Dev repository,
and records the immutable image digest and scoped file hashes. It repeats the
GitOps checks around registry and runtime reads; a branch move requires a new
capture. Unrelated or ambiguous resources are never silently included.

Existing admitted artifacts use their existing inspection authority. Historical
completed native provenance can be inspected without renewing its expired build
authority, while current configuration and grants still apply.

A pre-existing image without an RCP build record can receive distinct
**restoration-only image evidence** through an existing operator-configured
read-only registry observer for that exact component and repository. RCP first
records protected baseline roots, then queues the existing bounded observation
job. A successful scan receipt alone is insufficient: the exact baseline root
needs a distinct typed `IMAGE_CONTENT` proof of the complete, available
manifest/index and all required child/config/layer bytes, with fresh causal
job/attempt/fence/report evidence. The proof records the full expected descriptor
set; missing descriptors cannot disappear when a scan reaches its limits.

Incoming referrers and repository discovery retain their own status. For example,
a registry without a native referrers API may provide all bytes of the image while
its referrer inventory remains `PARTIAL`. A client-maintained fallback index does
not establish metadata completeness. The receipt preserves those blockers and
never enables garbage collection. A generic successful or partial inventory
without the explicit typed image-content proof cannot establish availability.

The server does not infer credentials from an
image URL or broaden an observer's configured scope.

This evidence does not create a `DeliveryArtifact`, claim native/GitHub build
provenance, or make the image eligible for candidate execution or production.
Missing authority, missing bytes and partial closure remain explicit failures.
Registry inventory completeness and deletion permission remain separate. A
valid empty fallback index with an omitted untagged referrer still leaves the
metadata inventory incomplete; it does not falsify a separate proof that all
bytes of the exact requested image were read successfully.

After registry observation, every configured runtime role must match the exact
baseline image, current deployment generation, positive replicas, Running/Ready
pods and immutable image IDs. Flux must be READY at the common pinned GitOps
head. The opaque runtime binding covers the effective API/TLS/auth configuration.
The final save rechecks the current owner, hold, selection, generation, lease,
coverage and configuration. Reads happen outside the storage transaction.

Only minimal typed evidence is stored. Combined proof and result, including a
failure result, cannot exceed 256 KiB; the limits are 16 components, 64 roles and
512 pods. No roles are dropped to fit. Raw provider payloads, pod environment
variables and runtime credentials are excluded.

## Activation and protection

New candidate activation requires the latest successful matching capture no
older than five minutes. Its immutable proof freezes the capture identity and
hash. Before the first candidate effect, changed baseline values or authority
block writes. After its own activation begins, the original capture becomes
historical restoration material; it is not a claim that the old image is still
running. Historical activation receipts without a capture cannot acquire new
write or restoration authority through replay.

Environment and operation queries report `baseline_capture_id`,
`baseline_capture_status`, `baseline_capture_observed_at` and
`baseline_capture_current_valid` separately from candidate runtime and baseline
intent. Current validity is a fresh, scoped observation, not a durable promise
of cluster health or an atomic cluster snapshot.

Owned retention roots protect captured images while the reservation remains
active, uncertain or quarantined. Failed, expired or superseded captures do not
silently remove protection. Releasing an ordinary manual pin does not release
baseline roots. Backup restoration quarantines capture authority; portable
configuration exports carry no execution rights. Global garbage collection
remains disabled pending the separate inventory and deletion-fence work.

## What comes next

Capture does not restore Dev, accept functional tests, release a hold, approve a
merge or promote production. Unknown original write outcomes still need exact
reconciliation. Restoration needs a separately confirmed, reviewed Flux image
change, complete post-restore runtime proof and an explicit scoped unlock.

[Grouped candidate proposals](DEV_GITOPS_BATCH.md) use this capture's current
selectors separately from desired candidate repositories and combine sibling
image changes in shared files. Capture and proposal preparation do not complete
the restoration lifecycle. The governing human-only protected Flux merge policy
remains in force.

The separate [complete baseline restoration lifecycle](DEV_RESTORATION.md) uses
a new recovery epoch and selection, exact-plan attributed confirmation, external
human-only reviewed Flux merges, fresh all-original availability/runtime proof
and explicit scoped unlock. Candidate expiry never renews candidate rights.
