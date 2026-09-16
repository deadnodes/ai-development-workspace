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
