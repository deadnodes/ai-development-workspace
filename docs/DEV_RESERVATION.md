# Candidate Dev reservation

`reserve_candidate_dev` prepares exclusive RCP-managed use of a shared DEV/TEST target by an already successful native main candidate. It does not deploy that candidate, accept its tests, promote it, restore the baseline, or grant GitOps merge permission. Receipt status `SUCCEEDED` means the request was accepted; its phase is `RESERVED`.

Product scope, an attributed actor, the candidate operation ID, environment ID, an opaque request key of 16–128 bytes, and exact current `expected_selection` (`composition_id`, `operation_id`, uint64 `generation`) are required. All candidate applications must belong to the selected shared baseline and have configured PR-only DEV/TEST bindings. The baseline target must still match. Current candidate source/gates, recipes/configuration and grants are checked; source identity is observed outside the transaction. Trusted-local attribution is not authenticated human approval.

Reservation atomically advances selection generation and server-owned reservation epoch. Existing desired composition/operation IDs, composition/revision history, Dev build configuration and desired/reconciled/runtime maps stay unchanged. The typed immutable proof records the candidate, baseline token and Dev targets. Environment state exposes `dev_reservation`, including owner, receipt ID and reconciliation requirements.

A hold denies generic selection, assembly/reconciliation, feature patches, automatic composition refresh, target configuration, environment updates and new environment-bound deployments. Existing native and non-native, standalone and parent/child deployment execution is fenced at admission, external mutation intent and save boundaries. Operations capture the current epoch; releasing a hold never restores older operation rights. Explicit new operations capture the current epoch after release. Pure external worker build/publication jobs without an environment or managed SourceProof remain available for parallel source work and gain no deployment rights. Git/runtime observations and application source editing remain available.

Unknown GitOps writes, open/unconfirmed PRs, unresolved build dispatches and running/uncertain managed publications prevent reservation even after lease expiry or operation cancellation. Source ref mutations first commit an intent atomically against the epoch. Unknown source outcomes recover only through exact read-only ref/composition verification; writes are not automatically repeated. Unprovable outcomes remain explicit reconciliation blockers. This slice adds no reconciliation/unlock protocol.

`release_candidate_dev` requires the same attributed owner, original reservation receipt ID, fresh exact selection token and another request key. Its `SUCCEEDED`/`RELEASED` receipt removes exclusion and advances selection generation, preserving baseline intent and current observations. It performs no deployment/restoration. No TTL unlock exists. Cancellation, expiry and restoration cannot silently release a hold. Restored active holds are quarantined: historical replay is allowed, blind release is denied. Portable workspace export/import strips epochs and execution state. Malformed/foreign release receipts cannot remove a hold.

Exact actor/Product/payload/key replay is a historical read even after release, reopen or restore. It returns the same receipt without new audit, selection changes or rights. Conflicting reuse rejects. Reserve/release keys share one Product-scoped receipt namespace.

This excludes RCP-managed commands; it is not physical cluster isolation against external Git/Kubernetes writers or externally reopened closed PRs. Native Prod promotion remains denied. Dev activation uses the bounded protocol below; runtime testing and verified restoration are still separate, unfinished capabilities.

## Activate an existing candidate in Dev

`deploy_candidate_dev` accepts Product and attributed actor plus only `reservation_operation_id`, a new `request_key` (16–128 bytes), and exact `expected_selection`. The same owner must hold the active, nonquarantined reservation and have a fresh verified [complete physical baseline](DEV_BASELINE_CAPTURE.md) before activation. The server selects the completed candidate's admitted immutable native artifacts; callers cannot substitute a digest, build profile, source, target, approval or configuration.

The receipt is a new `DEV_CANDIDATE_DEPLOY` aggregate with new `DEPLOY` children. Each starts at `INSPECT` with an exact existing artifact and the original effective Prod build/main-source proof. Only the execution destination changes to the reserved DEV/TEST PR target. There are no new builds, publications, source writes, direct GitOps writes, merges or mutations of the original candidate. Global Dev build configuration, shared baseline IDs/maps and original provenance stay intact. Request replay returns historical records before current configuration/source checks and grants no new authority.

An immutable proof links the reservation/epoch/selection, original candidate and component artifacts to the new children and exact Dev destination. Dedicated guards recheck this relation, recipes/grants/configuration, causal source/build checks, registry eligibility and read-only original-main ancestry at worker boundaries and before effects. A deleted author branch or a newer main descendant does not invalidate the original pinned main artifact. Generic operations still cannot use the hold.

Before its first proposal each child commits a durable `APPLY_UNCERTAIN` intent and revalidates immediately before invoking the bounded PR provider. An unknown response may only use the optional GET-only `GitOpsProposalLookup` capability. An absent PR, partial branch/commit, ambiguity, incomplete evidence or unavailable capability remains uncertain and never authorizes another proposal. Production lookup validates exact numeric repository identities, operation branch/base, marker, single parent, original base and complete byte-preserving image-field tree/blob patch; unrelated edits or mode/path changes reject recovery. A known PR is observed read-only against its pinned head. Literal GET-only lookup requires an already cached usable installation token; inability to obtain it without refreshing authentication is an unresolved lookup, not retry permission.

New activation proposals require `GitOpsGuardedPRProvider`; an unguarded legacy proposer is never a fallback. A server-owned pure-store callback checks the current lease, reservation, configuration and causal RCP gates immediately before each physical non-GET/HEAD HTTP send, including internal ref/commit/PR writes, redirects and authentication POSTs. The callback performs no provider IO; original-main source preflight remains outside the compound proposal. Revocation after one request suppresses later mutation sends. The already in-flight request can still have an uncertain effect. Before initially admitting a returned PR head, the same complete GET-only lookup must agree with its number, URL and head; a successful POST response alone is insufficient.

The cached token must belong to the exact GitOps connection. Original-main verification can use a different source connection and does not necessarily populate that cache. After a process restart, lookup may remain unresolved until an otherwise permitted ordinary authenticated GitOps observation populates the exact connection's cache; this protocol does not refresh authentication inside GET-only lookup or acquire new credentials.

Durable intent cannot revoke an HTTP mutation already in flight. Late invalidation suppresses further writes and successful acceptance of the result; the uncertain intent and reservation remain visible. A partial multi-component outcome never causes automatic rollback or release. Once an activation is admitted, blind `release_candidate_dev` is denied even after failure, cancellation, closure or merge of its PR. There is no TTL unlock. Verified restoration/successor release is not implemented by this slice. Restored activation records and holds are quarantined; replay is historical only.

Environment/operation queries expose `candidate_deployment_id`, status and desired GitOps state separately from baseline intent. The aggregate reaches `GITOPS_APPLIED` only when every child confirms the exact desired GitOps result. This means `PENDING_RECONCILIATION`: a prepared PR is not a merge, a merge is not healthy runtime, and neither is Dev acceptance or permission to promote Prod.

Existing eligibility and writer limits still apply. The original native candidate's one-hour authority deadline remains a fence even after its build completes; this protocol does not grant arbitrary-age reuse. The existing Helm image-field selector matches the requested repository in current documents and fails closed when Dev currently uses a different repository; this slice does not broaden it into a general manifest editor. Validate the actual target/profile mapping before a live pilot.

## Own OCI registry and GitOps image fields

The GitOps adapter accepts an explicit canonical image repository such as
`registry.example:5443/team/component` as well as GHCR. It writes that exact
repository with the admitted digest; it does not prepend `ghcr.io/`, fetch the
image, or send GitHub credentials to the registry. Native publication and
artifact inspection remain separate operations with their existing configured
registry authority. The Actions/GHCR artifact inspector remains GHCR-specific.

Explicit repositories must already match `CanonicalImageRepository`: registry
host and optional port, then a lowercase repository path, without scheme,
credentials, tag, digest, query, fragment or whitespace. Noncanonical explicit
identities are rejected rather than rewritten. Previously supported bare GHCR
owner/image inputs retain their existing interpretation; native artifact
identities use an explicit registry.

The writer, initial PR verification and uncertain-result lookup use the same
validated repository and exact byte-preserving YAML rendering. Root
`image.repository`/`image.digest` still requires one document. HelmRelease
`spec.values.image.repository`/`spec.values.image.tag` still selects only
resources already using the requested repository; all selected roles must
agree on the current version. A candidate from a different repository cannot
silently replace the existing Helm repository. Configure and verify the target
mapping separately before activating such a candidate. Unrelated YAML bytes,
resources and files remain outside the writer's authority.

Invalid proposal identity, mapping or pinned input is rejected before a
mutating HTTP request, including authentication or ref creation. Accepting a
private repository here grants no registry access, GitOps merge permission,
runtime acceptance, restoration or production promotion.

An applied candidate can request a separate server-owned
[`verify_candidate_dev_runtime`](DEV_RUNTIME_VERIFICATION.md) proof for its
configured complete workload groups. That point-in-time runtime check does not
accept functional tests, restore the baseline or release this reservation.

Before candidate activation, use [`capture_candidate_dev_baseline`](DEV_BASELINE_CAPTURE.md)
to record the complete physical shared baseline and available original images.
A reserved desired composition alone does not establish restoration evidence.
