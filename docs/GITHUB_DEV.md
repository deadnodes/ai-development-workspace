# GitHub App → Actions → GHCR → DEV GitOps

This is the first execution slice, not a general CI or GitOps engine. The Control Plane orchestrates a configured GitHub Actions workflow and records its evidence. It does not execute builds or store image bytes. Production deployment is excluded.

## Connection and credentials

Create a dedicated GitHub App (webhooks can be disabled for this polling slice). Grant repository Metadata read, Contents read/write and Actions read/write. For optional Codex review import, also grant Pull requests read and Issues read; see [PR review workflow](PR_REVIEWS.md). Install it on the selected SOURCE and GITOPS repositories. Repository discovery uses the installation and stores GitHub numeric repository IDs. Discovered repositories live in the instance Repository Registry. Multiple Products can attach the same registered repository. Product attachment is explicit; an installation's visibility is not an automatic product attachment. Feature and Integration repository lists narrow that selection.

Configure App ID, Installation ID, API URL (normally `https://api.github.com`) and a private key **reference**. App ID and installation ID are identifiers, not secret values. The RSA PEM remains outside database records and audit. The adapter signs a short-lived JWT and exchanges it for an installation token, cached only in memory until expiry. The local authenticated `gh` session is a setup/research tool, not the running application's credential backend.

Supported secret references are `env:RCP_…` and `file:/run/secrets/…` (or a path beneath explicitly configured `RCP_SECRET_DIR`). In Compose, `RCP_SECRET_DIR_HOST` selects a host directory mounted read-only at `/run/secrets`; default `.secrets/` is excluded from Git and image builds. Arrange file ownership so container UID 10001 can read the PEM while other host users cannot. Never paste PEMs or installation tokens into a command's data or workflow inputs.

The GitHub API destination defaults to `api.github.com`. An enterprise/test endpoint requires an operator allowlist (`RCP_GITHUB_API_URL` or `RCP_GITHUB_API_ALLOWED_HOSTS`); a UI field alone cannot redirect an App JWT to an arbitrary server.

Test the connection, discover repositories, and attach the required repositories with APPLICATION, LIBRARY, MIXED or GITOPS roles. A GitHub App with only Contents permission can read Git but cannot dispatch Actions; connection discovery alone is not proof of all execution permissions.

## Source binding and builds

Bind an existing Integration to its Product's Component, attached SOURCE repository, feature branch and base branch. Refresh Git reads actual branches and pins both comparison ends. CURRENT means not behind the observed base; BEHIND/DIVERGED/UNKNOWN remain explicit. Observation timestamps are evidence freshness, not proof that a branch will never move.

Configure a Component's build workflow and dispatch ref. `rebuild_missing` is an explicit opt-in (default false); missing or unverifiable artifacts cannot trigger a workflow unless enabled. The repository attachment's optional `base_branch` selects the comparison base independently of its observed default branch. A workflow definition must be reachable on the configured ref and registered on the repository's default branch. Pin/review that workflow independently of the source being built. The controlled workflow must build the exact `source_sha`, never just the latest feature branch. The initial workflow repository is the component's source repository.

Copy `examples/github-actions/build-image.yml` to `.github/workflows/build-image.yml` and `registry_contract.py` alongside it in the SOURCE repository. Set repository variable `RCP_IMAGE_REPOSITORY` to the lowercase GHCR image path. Set the workflow/build context to the actual component Dockerfile before use. The example uses repository-root Dockerfile and GitHub-hosted Linux runner. An existing workflow that also deploys production must not be reused as this build-only contract.

Inputs:

| Input | Meaning |
| --- | --- |
| `source_sha` | Full immutable source commit to checkout and verify |
| `image_tag` | Correlation tag chosen by the Control Plane |
| `image_repository` | Configured GHCR repository; workflow checks against `RCP_IMAGE_REPOSITORY` |
| `operation_id` | Persistent operation identifier; exact `run-name: rcp-<operation_id>` |

The workflow probes the registry first, verifies the revision label before reuse, builds when the manifest is missing, and verifies the pushed digest. A permission or network error fails the probe; it does not mean the image is absent. It writes an Actions artifact named `rcp-result-<operation_id>` containing `result.json`:

```json
{
  "operation_id": "operation-id",
  "source_sha": "full-source-sha",
  "image_repository": "ghcr.io/example/component",
  "image_tag": "control-plane-tag",
  "digest": "sha256:64-lowercase-hex-characters",
  "available": true,
  "observed_at": "2026-09-16T12:00:00Z"
}
```

The adapter correlates the run and validates report identity. It must never choose an unrelated latest successful run. A durable dispatch intent is recorded before the network call. After an uncertain dispatch response, the worker searches by correlation rather than blindly dispatching again. An operation can require intervention if the dispatch cannot be reconciled.

## Private GHCR without a PAT

Public images can be inspected anonymously using the registry protocol. Private GHCR authentication is performed **inside Actions** with its ephemeral `GITHUB_TOKEN`, then fresh registry evidence is obtained through the authenticated Actions artifact API. The App installation token is not treated as a general GHCR registry password. Package access must be granted to the workflow repository. A report is timestamped evidence, not permanent availability; old/deleted reports do not prove that the image still exists.

The Control Plane retains original artifact/provenance records when an image goes missing. Rebuilding can produce a different digest; original and rebuilt records remain distinguishable. Source equality does not imply bit-for-bit reproducibility. A failed private probe must remain visible rather than silently substituting anonymous access or a long-lived PAT.

## DEV GitOps mapping

Use a configured DEV environment and explicit direct-commit policy. The repository role must be GITOPS and all referenced objects must belong to the Product. The requested source, workflow, mapping and artifact are recorded with the operation so edits to configuration cannot silently change an in-flight deployment.

For a Flux HelmRelease, a configurable path could be `environments/dev/backend.yaml` in your GitOps repository. The supported alternative image mapping is `spec.values.image.repository` plus `spec.values.image.tag`. When the chart renders `repository:tag`, digest pinning therefore uses `tag: rcp@sha256:…`, yielding the valid immutable image reference `repository:rcp@sha256:…`. Adding an unused `image.digest` field would not deploy the intended image and is not sufficient validation.

The adapter supports a bounded image mapping, checks the existing Git head/blob and changes only the configured image values. GitHub Contents API enforces atomic expected-blob-SHA comparison; branch head is a preflight check, not an atomic branch lock. An intervening edit of that file fails the write; unrelated concurrent file changes are preserved. It never force-pushes. The commit carries operation correlation for audit/recovery. No Kubernetes object is patched directly.

GitOps publication completes as **GITOPS_APPLIED / PENDING_RECONCILIATION**. The operation may succeed at that boundary, but environment runtime remains unverified. Only a later Flux/runtime observation can justify DEPLOYED.

## Validation and live acceptance

Normal tests use deterministic fake providers and HTTP test servers, including errors, concurrent claims and restart recovery. They must not need GitHub credentials. Opt-in provider tests require explicit test configuration and perform documented read operations. They are not equivalent to the full live acceptance run.

Full milestone acceptance requires one real missing-image operation with recorded SOURCE SHA, Actions run URL/ID, GHCR digest, GitOps commit and persisted step history visible through UI, API and MCP. A connected `gh` session alone does not supply a GitHub App PEM. If App setup, package permissions or workflow configuration are unavailable, record the blocker and leave the live-validation integration incomplete.

Official references: [App installation tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app), [workflow dispatch](https://docs.github.com/en/rest/actions/workflows), [GHCR authentication](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry), [publishing from Actions](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images).

The opt-in read test uses `RCP_GITHUB_LIVE_TEST=1`, `RCP_GITHUB_APP_ID`, `RCP_GITHUB_INSTALLATION_ID`, `RCP_GITHUB_PRIVATE_KEY_REF`, optional `RCP_GITHUB_OWNER`/`RCP_GITHUB_API_URL`, and required `RCP_GITHUB_TEST_REPOSITORY` (numeric ID or owner/name), `RCP_GITHUB_TEST_BRANCH`. Run `go test ./internal/providers/github -run TestLiveAppReadOnly -v`. It discovers the installation, verifies membership, reads branches and compares pinned heads; it does not dispatch, push or deploy.
