# Read-only Flux and Kubernetes observation

Set `RCP_OBSERVERS_FILE` to a server-local JSON file. Mount it and referenced
credentials read-only in Docker. The server polls every 30 seconds, without
running kubectl or changing cluster resources. Omit the variable to disable.

```json
[
  {
    "product_id": "example",
    "environment_id": "dev-id",
    "application_id": "backend-id",
    "api_url": "https://kubernetes.example:6443",
    "ca_file": "/run/secrets/cluster-ca.pem",
    "token_file": "/run/secrets/cluster-token",
    "flux_namespace": "flux-system",
    "kustomization": "applications",
    "namespace": "example-dev",
    "deployment": "backend",
    "container": "backend"
  }
]
```

Alternatively use `client_cert_file` and `client_key_file` references for mutual
TLS. No plaintext credentials are stored in domain records. Tokens are reread
on each request so mounted service-account token rotation works. Client cert/CA
rotation requires server restart. HTTPS certificate verification is required;
redirects are not followed. Never mount an entire personal kubeconfig merely to
supply broad administrator access. Use credentials granting read-only access to
the configured Flux Kustomization, Deployment and namespace Pods.

Evidence is attached to the latest successful GitOps operation for the configured
Product/environment/component and appears through existing environment/context
HTTP and MCP reads (`runtime_observations`). `details` contains observed Flux
revision, actual runtime image IDs and expected commit/digest. An observation's
commit/digest identifies the deployment being checked; it is not a claim that
these were observed unless `healthy` is true and evidence matches.

Healthy requires Flux Ready at the current generation and exactly the operation's
GitOps commit, a current fully rolled-out Deployment, positive desired replicas,
and all selected Pods ready/running with the configured container running the
expected immutable digest. Missing RBAC, missing resources, wrong revisions,
wrong digests and empty/scaled-to-zero workloads never mean healthy. Deployment
selectors with matchExpressions currently remain unknown. Newer Flux revisions
are not inferred to contain an earlier commit. If a tag resolves to a multi-arch
index but Pods report a platform digest, the mismatch remains not-ready: platform
manifest provenance support is a separate extension.

Evidence is append-only on changes and at least once per minute while observed.
The existing runtime worker consumes the same evidence as externally recorded
checks. A configured observer does not itself trigger builds, GitOps writes or
releases. Before a successful Control Plane GitOps operation exists, it has no
operation to attribute evidence to and writes nothing.
