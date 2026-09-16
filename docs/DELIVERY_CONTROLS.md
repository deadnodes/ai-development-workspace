# Environment delivery controls

Open **Environments & delivery** in the sidebar, or follow the setup link from
Operations. All configuration is persisted in the application, shared with MCP,
and attributed in audit history.

1. Attach a source repository and create an application component. Libraries have
   a separate package workflow and are not Kubernetes deployment targets.
2. Configure the component's GitHub Actions workflow, trusted workflow ref and GHCR
   image repository. The workflow must implement [the build contract](GITHUB_DEV.md).
3. Add/select an environment and configure its GitOps mapping: attached GITOPS
   repository, branch, file path, image repository YAML field and digest/tag YAML
   field. For a Flux HelmRelease use `spec.values.image.repository` and
   `spec.values.image.tag` if those fields are consumed by its chart. These are
   structured YAML paths, not line numbers that change when a file is edited.
4. Bind the Integration to its source branch. The normal deployment action resolves
   its source, reuses/builds the artifact, and commits the configured GitOps fields.
5. For a previously recorded build, use **Deploy existing artifact**. This action
   revalidates the selected source/artifact and publishes the exact digest without
   starting a replacement build. Missing or stale proof blocks the operation.

A GitOps write creates a real commit through the GitHub App. The operation records
its steps and commit. `GITOPS_APPLIED / PENDING_RECONCILIATION` means Flux can observe
that commit; it does not assert healthy pods. Production keeps its separate release
approval and verification flow.

## Existing GitHub workflows

The service does not infer image provenance from arbitrary workflow console logs
or assume that the newest tag is the right image. Existing artifacts must have the
recorded build evidence required by the Control Plane contract. A GitHub connection
alone does not configure a component workflow or import every registry tag.
An empty artifact list means no qualifying build has been recorded, not that the
registry has no images. Configure the contract before using artifact selection.

Human buttons and MCP execute commands use the same application validation. See
`GET /api/schema` and [CONTRACT.md](CONTRACT.md) for action argument schemas.

MCP named tool: `deploy_existing_artifact` with actor, integration_id,
application_id, environment_id, revision_id and artifact_id. The equivalent
`execute` action uses top-level integration_id and puts the last four IDs in data.
For private GHCR, current report evidence must be no more than ten minutes old;
expired proof blocks this no-build operation instead of dispatching CI silently.

Browser links include `?product=PRODUCT_ID` before the section hash, for example
`/?product=deadnodes#delivery`. Share the complete address. Product selection
preserves the Delivery/Operations section; browser Back/Forward restores the product.
Legacy links acquire an explicit product after opening.

## Delete a product

Overview → Delete product requires the exact product name. The shared API/MCP
command is `delete_product` with `product_id` and `data.name`. Product-owned
records are removed atomically; audit events, instance repository registry and
shared provider connections remain. External repositories, images, files and
running applications are untouched. Unfinished operations prevent deletion.
Export a backup first if you need to restore the removed development context.

## Advisory artifact retention

Delivery → Artifact retention configures a policy per component. `keep_last`
protects the newest distinct immutable images, `keep_current` protects the last
GitOps-applied image for each environment, and `keep_previous` protects previous
distinct images for rollback. This uses recorded observations, not a fresh registry
probe. No registry bytes or provenance are removed.

Agents use `execute` with action `configure_artifact_retention`, `product_id` and
`data: {application_id, keep_last, keep_current, keep_previous}`. Read evaluation
through MCP `get_artifact_retention {product_id}` or
`GET /api/products/{id}/retention`. Protected missing/unknown images produce
attention items in UI and MCP `get_attention_required`. A warning asks for registry
verification or an explicit rebuild; it never triggers cleanup or CI itself.

## GitOps pull requests

Configure an environment mapping with `gitops_mode: PR` (or choose PR in the UI).
The default DIRECT mode retains the existing commit behavior. PR mode builds and
validates the same pinned source/artifact, then writes only the configured image
fields on `rcp/gitops/<operation-id>` and opens a GitHub PR to the configured ref.
The GitHub App needs Pull requests write as well as Contents write for this mode.

The persistent deployment operation stays RUNNING / GITOPS_PENDING while the PR
awaits external review. `gitops_pr` contains the PR number, URL and pinned head;
UI/API/MCP operation reads show the same evidence. The service never merges it.
A closed unmerged PR cancels the operation. An altered PR head or unexpected
post-merge image prevents acceptance. After merge, the target branch must contain
the exact expected image repository and digest before GITOPS_APPLIED is recorded;
Flux/runtime observation is still required for DEPLOYED. Production keeps the
existing explicit release-candidate approval and verification requirements.

Multi-document Flux files are supported for the HelmRelease image mapping. The
writer selects all HelmRelease documents whose `spec.values.image.repository`
exactly matches the configured component image, requires their current tags to
agree, and updates them together in one file commit. Other images and non-target
resources (for example Service) are untouched. Scalar replacement preserves
comments and unrelated file bytes. Missing targets, mixed current versions,
anchors, aliases and multiline image values fail closed. Head/blob concurrency
checks still cover the complete shared file. This does not enable a mapping or
launch a deployment automatically.
