# Read-only Kubernetes runtime synchronization

The service calls the Kubernetes HTTPS API directly. It does not invoke `kubectl`, run authentication shell commands, read Kubernetes Secrets, or patch workloads. The developer's kubeconfig supplies connection/TLS/authentication material; it is never stored in the application database or returned by MCP.

## Local startup

Set `RCP_OBSERVERS_FILE` to a JSON array of targets. Use an explicit context for each target:

```json
[
  {
    "product_id": "your-product",
    "environment_id": "your-dev",
    "application_id": "your-component-id",
    "context": "your-kubeconfig-context",
    "namespace": "your-namespace",
    "deployment": "your-deployment",
    "container": "your-container",
    "flux_namespace": "flux-system",
    "kustomization": "your-kustomization"
  }
]
```

`flux_namespace` and `kustomization` are optional together. Configure a separate target for every Deployment/container pair, including multiple workers of one component. Libraries need no runtime target.

With `context` set, loading follows an optional target `kubeconfig` path, then `KUBECONFIG`, then `~/.kube/config`. Path lists merge with the first named entry winning. Context is explicit; changing the user's current-context does not redirect existing targets. Inline CA/client certificates and tokens, certificate files and token files are supported. Relative file references resolve beside their kubeconfig. Exec/auth-provider plugins and insecure TLS are rejected; use a certificate/token kubeconfig for this initial direct-API implementation.

```sh
RCP_OBSERVERS_FILE=/absolute/path/runtime-observers.json ./bin/release-control
```

Existing explicit API URL and certificate/token file target configuration is also supported.

## Docker using the system kubeconfig

Keep the observer JSON under the existing secret-directory mount, using IDs from the running instance. Add the read-only kubeconfig overlay:

```sh
export RCP_KUBECONFIG_DIR_HOST="$HOME/.kube"
export RCP_OBSERVERS_FILE=/run/secrets/runtime-observers.json
export COMPOSE_FILE=compose.yaml:compose.kubernetes.yaml
docker compose --profile app up -d --build
```

If using workspace mounts too, retain `compose.workspace.yaml` in `COMPOSE_FILE`. The overlay mounts the directory at `/run/kube` read-only and sets `KUBECONFIG=/run/kube/config`. Certificate files referenced by absolute paths require corresponding read-only mounts; relative files under `.kube` work with the directory mount. Do not copy credential contents into domain configuration or Git.

For a dedicated installation, use an observer kubeconfig authorized only for GET of configured Deployments/Flux Kustomizations and GET/LIST of ReplicaSets and Pods in configured namespaces. The service performs only GET requests even when an existing developer kubeconfig has wider privileges.

## UI / API / MCP

**Environments & delivery → Runtime versions → Sync Kubernetes** queues a persistent read-only operation. The Feature's **Environments** tab shows runtime for relevant components. The browser updates while the operation completes; server snapshots remain after restart. There is no implied background inventory schedule: use Sync Kubernetes for a fresh inventory. The older release-verification observer remains separate.

MCP:

```json
{"name":"refresh_environment_runtime","arguments":{"actor":"agent/runtime-check","product_id":"your-product","environment_id":"your-dev"}}
```

Optionally supply `application_id` to limit the observation to one component. Read `get_operation` and `get_environment_state` afterward. HTTP uses POST `/api/commands` with `action: refresh_environment_runtime`, actor/product_id and `data: {environment_id, application_id?}`; GET `/api/environments/{id}/state` returns targets and snapshots.

## What is compared

- **Expected**: freshly read configured GitOps image fields and Git commit.
- **Workload**: Kubernetes Deployment template image and observed generation.
- **Running**: selected container's image and imageID, readiness and phase. Pods must belong to the Deployment through controller UID references, not merely matching labels.
- **Actions**: bounded recent successful workflow metadata, including URL, HEAD SHA and branch. A successful workflow is not proof of a published image or its source; verified artifact records are linked separately by digest and image repository.

Matching mutable tags alone cannot establish image-byte identity. Missing data stays UNKNOWN; failed/partial API reads preserve errors and do not reuse old health as current. Digest comparisons are conservative about multi-platform image indexes versus per-platform runtime digests. Snapshots never authorize deployment or advance release readiness.

Validation: `make check`, `node web/runtime-check.mjs`; normal tests use deterministic HTTPS fixtures and require no live Kubernetes credentials.

## Inside Kubernetes: Pod ServiceAccount

Use `deploy/overlays/in-cluster` instead of the base deployment. It selects a dedicated ServiceAccount, enables the projected Pod credentials, mounts the target configuration and grants read-only access in the installation namespace. Fill `runtime-observers.json` with real Product/Environment/Component IDs before rendering the overlay; its initial empty list intentionally observes nothing. The usual image, database/access settings and namespace must still be configured as described in the deployment guide.

A target for the current cluster needs no kubeconfig, API URL or credential paths:

```json
{
  "in_cluster": true,
  "product_id": "your-product",
  "environment_id": "your-dev",
  "application_id": "your-component-id",
  "namespace": "release-control",
  "deployment": "your-deployment",
  "container": "your-container"
}
```

The service connects directly to `KUBERNETES_SERVICE_HOST:KUBERNETES_SERVICE_PORT`, validates TLS against `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` and reads the projected `token` file for **every request**, including after rotation. No kubeconfig or kubectl is needed inside the Pod. See [Kubernetes API access from a Pod](https://kubernetes.io/docs/tasks/run-application/access-api-from-pod/).

Selection rules:

- Explicit kubeconfig/context or API credentials keep their existing behavior; failures never silently switch clusters.
- `in_cluster: true` explicitly selects the Pod identity and cannot be mixed with explicit connection fields.
- With no explicit target connection and no `KUBECONFIG` environment variable, Kubernetes service environment variables automatically select in-cluster authentication. Outside a Pod the existing system kubeconfig path remains the fallback.
- Target namespace remains explicit. The service does not scan every namespace or grant itself permissions.

For workloads in another namespace, create the same Role **in that target namespace** and a RoleBinding there whose subject is `release-control-runtime` in the Control Plane's namespace. Do not use a ClusterRoleBinding merely to observe one namespace. The optional Flux observation additionally needs a Role in its namespace granting `get` on `kustomizations` in API group `kustomize.toolkit.fluxcd.io`, ideally restricted with `resourceNames` to configured Kustomizations, and a matching RoleBinding. No Secrets, exec, writes, or application data permissions are required.

Render for review with `kubectl kustomize deploy/overlays/in-cluster` (this is an operator-side manifest tool; the service never invokes it). Apply through your existing GitOps deployment process after filling configuration. The base deployment still disables ServiceAccount token mounting unless this overlay is selected.
