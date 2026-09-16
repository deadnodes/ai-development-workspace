# Dynamic environments and independently traceable feature composition

## Product boundaries
Every Product owns its own named Environments. Names are data, not an enum or mandatory promotion chain. Product A may use DEV/PROD while B uses DEV/TEST/STAGE/PROD. Adding a target records cluster/namespace coordinates; it does not create a cluster or namespace. Independent products may share a physical cluster, but their logical plans and feature selections remain isolated. Cross-product bundles would be an explicit future object, never an implicit merge of unrelated products.

Application identifies a deployable part of a product, linked to a repository and optional monorepo path. Multiple applications can share a repository. Environment identifies where; Composition identifies what should run there.

## Source identity versus integration branch history
IntegrationRevision is an immutable capture of one integration's source for one repository: branch, pinned base SHA, head SHA, and ordered commit identifiers. A new capture creates a new revision; it never replaces an earlier revision. Multiple repositories yield separate captures for the same integration.

A composition pins a base commit and an ordered selection of revision IDs for every application's repository. The future Git worker derives a disposable `generated/...` branch from those inputs. Multiple features may therefore coexist in the same integration branch while their individual source history remains independent of merge commits. Applications sharing a repository must agree on its base, target branch and ordered revision selection.

Example: DEV revision 12 contains main@M + PAY/INT-03@P + AUTH/INT-02@A. DEV revision 13 can contain main@M2 + PAY/INT-03@P2, excluding AUTH. Revision 12 and the P/A source captures remain unchanged. Recomposition starts from the chosen base; removing AUTH is not a chain of guessed revert commits.

Recording SHAs alone does not retain Git objects. The Git adapter must keep protected immutable refs for captured source versions (for example refs/rcp/integrations/<integration>/<revision>) before branches are deleted or rewritten. Capture is currently client-reported, not verified against a Git provider. Ancestor relationships, exact feature commit membership and merge conflict freedom remain unverified until that adapter exists. A feature branch that imports unrelated dev work cannot be cleanly separated merely from its name; feature source branches should start from main and dependencies must be declared.

## Planning slice implemented now
- Create applications and arbitrary environment targets.
- Record immutable per-integration repository revisions with full SHA identifiers.
- Plan a named composition with application rows, exact base, generated branch and selected revisions.
- Preserve application, environment and source revision snapshots in every plan.
- Select an existing immutable plan as the environment's desired composition; audit the change.
- Reject cross-product references, duplicate application/revision selections, incompatible same-repository plans and missing unreleased dependencies.
- Keep desired selection separate from reconciled/runtime observations. A selected plan is still `planned`, not deployed or healthy.

Selecting an older plan changes only desired intent. It is not a verified runtime rollback. Creating an empty revision selection allows a base-only application version. Feature gates/readiness do not prohibit planning unfinished work on DEV. Production eligibility is not inferred from an environment's name; execution/promotion policy must be configured before production automation is enabled.

## Execution slice next
On a deployment request: resolve and verify immutable source refs → isolated composition/merge preview → conflict report or generated commit → build each affected application → retain image digests and test evidence → update GitOps desired state → observe Flux reconciliation → observe Kubernetes rollout. A Deployment record must link the composition ID, repository outputs, artifact digests and observed outcome. Merge conflicts stop the operation for a human/agent resolution; they are not automatically guessed away. Partial rollout remains distinguishable from success.

The current UI/API/MCP implement planning and source attribution. They do not perform Git merges, builds or external deployments. Work is tracked in Feature `rcp-environments`, integrations `rcp-env-plan` and `rcp-env-execution`.
