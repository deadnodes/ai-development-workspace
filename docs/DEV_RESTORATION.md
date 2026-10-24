# Restore the complete shared Dev baseline

Restoration returns **all** components of the immutable physical
[baseline capture](DEV_BASELINE_CAPTURE.md), including untouched components,
to their original repositories/digests. Explicit successor release settles only
the same owner's hold. It preserves original artifact/source/build provenance,
desired composition/operation IDs and the candidate's one-hour execution fence.

## Exact shared commands

All four commands require explicit nonblank attributed `actor` and `product_id`.
Generic HTTP `POST /api/commands` and MCP `execute` use
`{action,actor,product_id,data}`. Named MCP tools use the action name and flat
fields. Named HTTP POST bodies use flat fields; any body Product must match
the path Product.

| Action | HTTP suffix under `/api/products/{id}/candidate-dev/` | Required data fields |
| --- | --- | --- |
| `request_candidate_dev_restoration` | `request-restoration` | `reservation_operation_id`, `request_key`, `expected_selection` |
| `confirm_candidate_dev_restoration` | `confirm-restoration` | `restoration_operation_id`, `expected_plan_hash`, `request_key`, `expected_selection` |
| `verify_candidate_dev_restoration` | `verify-restoration` | `restoration_operation_id`, `request_key`, `expected_selection` |
| `release_candidate_dev_after_restoration` | `release-after-restoration` | `restoration_operation_id`, `restoration_verification_operation_id`, `request_key`, `expected_selection` |

Selection is exactly `{composition_id,operation_id,generation}`, with an exact
uint64 generation. Opaque request keys contain 16–128 bytes and share the
Product receipt namespace. Exact actor/Product/action/payload/key replay is a
historical read before IO, including after expiry/release/restart. Conflicting
reuse rejects. Additional scope, duplicate/case-alias keys, manifests, images,
selectors, caller health/trust/approval and timestamps are rejected. Legacy
commands retain their existing attribution default.

```json
{"action":"request_candidate_dev_restoration","actor":"agent/developer","product_id":"your-product","data":{"reservation_operation_id":"original-hold","request_key":"restoration-request-0001","expected_selection":{"composition_id":"shared-baseline","operation_id":"baseline-operation","generation":42}}}
```

## Request, inspect, confirm, externally merge, verify, release

1. Request under the exact active unquarantined original hold. RCP advances
   reservation epoch and selection generation. Read the new current selection;
   the original hold's immutable reserved selection remains historical. Old
   environment-bound candidate and recovery workers are fenced.
2. Inspect the asynchronous operation through `get_operation`: stages include
   `RECONCILE_EFFECTS`, `PREPARE_PROPOSALS`, `WAIT_EXTERNAL_MERGE`, then
   `GITOPS_APPLIED`. Registry availability waits appear in `RECONCILE_EFFECTS`
   details. Every known original/earlier
   intent needs exact cached-token GET-only settlement proof before proposals.
   Missing, ambiguous, partial, open or reopened effects retain the hold.
   Protect roots before fresh availability checks for every original image.
3. Wait for the complete `dev_restoration_plan.hash`. Inspect every original
   component, no-write member, PR repository/ref/number/head and image. Submit
   a **separate** confirmation with that exact hash. The immutable receipt's
   trust grade is `ATTRIBUTED_CONTROL_CONFIRMATION`: Actor and shared control
   token establish attribution only, not authenticated human text/voice identity.
   No raw transcript/audio/prompt is stored. Changed plan/head/configuration/epoch
   invalidates confirmation. An early external merge is an unapproved effect;
   confirmation cannot backdate approval.
4. The separately authorized human-only reviewed protected Flux merge remains
   the application-change gate. RCP creates image-only proposals and observes
   exact contents/merge; it never merges or applies them. Restoration merge proof
   requires the provider's authoritative `merged_at` to be present and strictly
   later than the immutable confirmation's `confirmed_at`. Observing an open PR
   before saving confirmation cannot backdate approval of a concurrent merge.
   GitHub timestamps have second precision: the external human merger should
   wait at least two seconds after confirmation before merging. Missing, earlier
   or same-second merge timestamps cannot prove chronology, so RCP blocks and
   retains the hold rather than calling the effect approved. All original-image
   results within each physical group must be saved at one stable descendant
   head. Already-original members have typed no-write proof, never fabricated PRs.
5. After all desired results are `GITOPS_APPLIED`, explicitly request a new
   verification. It checks **all** original components and configured API/worker
   roles: fresh availability, Flux READY/current revisions, Deployment generation,
   positive replicas, all Running/Ready pods and exact immutable imageIDs.
   Current observer/TLS/configuration and settlement bindings must agree. A newer
   pending/failed generation immediately invalidates an older PASS.
6. Explicitly release using the restoration and latest successful verification
   operation IDs and current selection. Release requires matching confirmation,
   complete proof no older than five minutes, current authority and final cached
   GET-only settlement of every intent. The transaction settles only this hold
   and advances epoch/selection. `RESTORATION_VERIFIED` and `RELEASED` remain
   distinct from desired-state success, functional acceptance and Prod authority.

This is point-in-time RCP fencing. GitHub reads plus a database transaction cannot
prevent an authorized external writer reopening/merging after the final read;
the governing human-only merge procedure remains essential.
Timestamp ordering proves only observed effect chronology. It does not establish
authenticated human identity, text/voice approval or stage08 trust.

## Expiry, retries and protection

An expired candidate can recover through a **new explicit request**, current
selection and current scoped grants. It gains no renewed candidate/source/build/
publication or Prod rights. Each recovery attempt grants at most one hour of
proposal authority, checked before every physical mutating send, authentication
and redirect against lease/deadline/hold/owner/generation/epoch/selection/config/
grants. After expiry, GET-only reconciliation, separately requested fresh proof
and explicit local scoped unlock use their own current authority; proposal
writes cannot resume.

A new request/key/selection creates another attempt only after every earlier
intent is exactly settled as merged, closed-unmerged or proven no-intent/no-write.
Unknown/open/in-flight effects block renewal. Selected-file drift, changed
destination/configuration, owner transfer and quarantine require separate
reconciliation scope. Selectors derive only from original mappings and exact
proven candidate/earlier restore patches; unrelated other-file head movement may
be incorporated. There is no automatic retry, rollback, cancellation, TTL unlock
or release-and-reserve shortcut. Ambiguous sends use cached-token GET-only full
proof without refresh or recreation.

Original/candidate/attempt roots stay protected while active, unknown, partial,
failed, expired or quarantined. Only an exact validated successor settles owned
reservation roots. Settlement grants no physical deletion; global GC stays
disabled. Backup restore quarantines recovery parents/children, confirmations,
proofs/releases and malformed orphan references. Historical replay cannot unlock
quarantine; portable exports strip execution/approval authority.

## Bounds and source acceptance

There are at most 256 attempt/effect references per hold. New authority, plan,
result, confirmation and settlement proofs, including failures, are bounded to
256 KiB, 16 original components, 64 roles and 512 pods. No failed member is
omitted. Grouped-writer bounds remain 16 members/files/groups, path depth 16,
1 MiB/file, 4 MiB each original/rendered bytes, 64 KiB batch, 128 proof reads,
16 MiB responses and 2,048 children/tree directory. Persist typed refs/hashes and
minimal observations, not raw YAML/provider payloads/credentials.

Known-native historical inspection or scoped `IMAGE_CONTENT` proves original
bytes without fabricated `DeliveryArtifact` provenance. Missing bytes/authority
or required closure blocks. Metadata `PARTIAL`, unknown referrers and incomplete
global inventory remain incomplete; image availability grants no GC permission.

Native pipeline acceptance uses actual Git, BuildKit/spool/publisher, Distribution
and isolated PostgreSQL where enabled; GitHub/Kubernetes peers and external
merges are fixtures. Transport stub tests prove serialization/strict inputs only.
Source checks do not demonstrate a live installation, trusted human text/voice
issuer/session/bridge, agent-merge policy cutover or full RCP-011 completion.
Stage08 trust, functional acceptance, selective Prod and live shared-Dev scenarios
remain separate work. No new service, credentials or live rollout is authorized.
