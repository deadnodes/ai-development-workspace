# Verify an activated Dev candidate's runtime

`verify_candidate_dev_runtime` collects a server-owned point-in-time proof for
an already applied `DEV_CANDIDATE_DEPLOY`. It does not accept functional tests,
restore the shared baseline, release its reservation, approve a merge or promote
production. The original candidate and its artifacts remain immutable. New
activations first require a [complete physical baseline capture](DEV_BASELINE_CAPTURE.md);
the later candidate runtime proof does not replace that pre-candidate evidence.

## Configure complete coverage

Each candidate component needs an operator-configured complete group of
Deployment/container observers in `RCP_OBSERVERS_FILE`. Add the same nonempty
`verification_group` to **every** role belonging to that component, including
workers. Different components may have their own group. Example target:

```json
{
  "product_id": "your-product",
  "environment_id": "your-dev",
  "application_id": "your-component-id",
  "context": "your-kubeconfig-context",
  "namespace": "your-namespace",
  "deployment": "your-worker",
  "container": "your-container",
  "flux_namespace": "flux-system",
  "kustomization": "your-kustomization",
  "verification_group": "complete-component-v1"
}
```

A group is the operator's declaration of complete configured coverage; it is
not automatic discovery of every workload in a cluster. Map all required
roles before using verification as evidence. The caller cannot choose a
subset. Missing, duplicate, mixed-group or informational-only roles cannot
produce a verified result. Group and target configuration cannot be supplied
through this command or MCP.

Context and namespace must match the frozen environment target. In-cluster
authentication resolves its context identity to `in-cluster`; the environment
must use that identity. An opaque fingerprint additionally binds the resolved
API endpoint and TLS/authentication material, so equal context labels cannot
hide a different connection. The public proof exposes no API URL, credential
value or credential/certificate file path. Verification needs explicit resolved
CA material; an observer using uninspectable default system trust remains
available for informational snapshots but cannot certify this proof.

These are the existing read-only observers described in
[Kubernetes runtime observation](KUBERNETES_RUNTIME.md). No new Kubernetes
permissions or credentials are granted by a group. A changed group, role or
resolved connection invalidates the current truth of an older proof.

## Request a fresh check

The same attributed owner must hold the active, nonquarantined reservation.
The activation must have completed every exact GitOps child. Supply the
current selection token from the environment:

```json
{
  "action": "verify_candidate_dev_runtime",
  "actor": "agent/developer",
  "product_id": "your-product",
  "data": {
    "activation_operation_id": "your-activation-operation",
    "request_key": "unique-runtime-request-0001",
    "expected_selection": {
      "composition_id": "your-baseline-composition",
      "operation_id": "your-baseline-operation",
      "generation": 42
    }
  }
}
```

Named HTTP: `POST /api/products/{id}/candidate-dev/verify-runtime`. Named MCP
uses the same action and fields. No caller-provided `healthy`, observations,
targets, digest, approval or override is accepted. The returned
`DEV_CANDIDATE_RUNTIME_VERIFY` operation performs reads outside the metastore
transaction and revalidates authority and lease before saving its outcome.

Exact request-key replay is a historical read, including after expiry or
restore. It never observes runtime again or restores old rights. Use a new
request key to perform another check. Admission of a newer verification makes
an older PASS noncurrent immediately, including while the newer request is
pending or after it fails. A late older result cannot supersede it.

## What is proved

The verifier pins one fresh GitOps head for each physical API authority and numeric repository/ref and
proves that it contains every activated child's historical GitOps result. Different credentials for the same API may share a head; numeric IDs at different API endpoints do not identify the same physical repository.
For a [grouped activation](DEV_GITOPS_BATCH.md), components sharing one physical
destination have the same proposal commit and PR, while retaining their own
artifact and file/blob evidence. Separate physical destinations retain their
own ancestry proofs. Each current target must contain its exact candidate
image/digest at the common head for that destination. A detectable branch move
during the read invalidates the check; it is not silently repinned. Historical
child commits and blobs are preserved, including when siblings share a Helm file.

Every configured role must report Flux READY at that common head, current
Deployment generation, positive replica count and a complete set of Running,
Ready pods. Workload/pod image references and immutable image IDs must match
the exact repository and digest. Mutable tags alone, zero pods, partial rollout,
missing roles, stale generations, wrong image hosts, unknown index/platform
digest relationships or API errors cannot establish a match. Caller-attributed
runtime observations and ordinary informational snapshots cannot mint this
proof.

Proofs fail closed above 16 components, 64 roles, 512 pod records or 256 KiB;
roles are never dropped to fit a limit. Only minimal scoped runtime/GitOps
metadata is retained, not raw Kubernetes payloads, pod environment variables or
application data.

Environment and operation queries expose `candidate_runtime_verified`,
`candidate_runtime_verification_id`, status and observation time separately
from baseline intent and existing maps. `true` requires the latest successful
`RUNTIME_VERIFIED` proof, still-current authority/configuration/coverage and an
observation no older than five minutes. This is evidence at observation time,
not an atomic cluster snapshot or a promise of future health. Replays, newer
failures, quarantine or changed authority cannot turn historical evidence into
current truth.

## What remains separate

Runtime agreement is not functional Dev acceptance. Required application
scenarios still need execution against the exact candidate and scope.
Verification does not remove the one-hour candidate authority fence, release a
hold or allow native production promotion. Backup restoration quarantines
execution authority; portable workspace configuration carries none.

Verified restoration still needs an exact pre-candidate baseline, available
original artifacts, a reviewed restoration PR, separate rollback confirmation
and a fresh post-restore runtime proof before a scoped unlock. Unknown effects
retain the reservation. The currently governing protected Flux merge policy
continues to apply.
