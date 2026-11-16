# Production signer

The standalone /usr/local/bin/prod-signer runs one reconciliation pass in the
PROD cluster, namespace supply-chain. It shares an image with RCP but does not
change the server entrypoint. RCP dev has only cosign-dev authority: no prod
Vault credential, role, route or signing key is available to the dev server.

Trust requires merged production Flux intent plus a pinned dev signature and
SLSA provenance: main- tag, approved builder ID, successful required CI checks,
matching subject digest and image revision label. A Flux-controlled backfill
entry can explicitly approve a legacy repository/digest with reason and PR URL.
Protect Flux main and backfill changes with review. Artifact integrity is checked
against Flux's sha256 digest; the signer trusts the Kubernetes API and Flux
controller to identify the reviewed artifact. A compromised prod Flux controller,
prod signer identity or prod Vault authorization can bypass this boundary.

The signer writes cosign signature and DSSE approval attestation tags into
prod-signatures, preserving the Flux repository in docker-reference. Predicate
type: https://deadnodes.dev/attestations/prod-approval/v1. Approval records Flux
revision/digest, file paths, image ref, and dev build/source or backfill approval.
Tags are appended idempotently; keep concurrencyPolicy: Forbid and prohibit
other writers to this repository. An existing valid prod signature skips work.

## Configuration

Use --config (or PROD_SIGNER_CONFIG); default /etc/prod-signer/config.json.
JSON is strict, including nested fields. Example (replace public PEM placeholders):

```json
{
  "flux_artifact": {"namespace":"flux-system","name":"flux-system","kind":"GitRepository"},
  "paths":["clusters/ovh-vpc-k3s-prod/"],
  "own_image_prefixes":["registry.deadnodes.internal/deadnodes/","ghcr.io/deadnodes/"],
  "registry":{
    "origin":"http://10.43.200.10:5000","insecure_http":true,
    "credentials_file":"/var/run/secrets/prod-signer-zot/credentials.json",
    "rewrite":{"registry.deadnodes.internal/":"","ghcr.io/":"mirror/ghcr.io/"}
  },
  "signatures_repository":"prod-signatures",
  "dev_public_key_pem":"REPLACE WITH DEV PUBLIC PEM",
  "prod_key":{
    "vault_addr":"http://vault.vault.svc.cluster.local:8200",
    "transit_mount":"transit","key_name":"cosign-prod",
    "public_key_pem":"REPLACE WITH PROD PUBLIC PEM",
    "auth":{"method":"kubernetes","mount":"k8s-deadnodes-prod","role":"cosign-prod-signer","token_file":"/var/run/secrets/vault-cosign/token"}
  },
  "builder_ids":["https://rcp-dev.tail9c31db.ts.net/builders/forgejo/v1"],
  "backfill_file":"clusters/ovh-vpc-k3s-prod/custom-resources/supply-chain/backfill.yaml",
  "metrics_file":""
}
```

The production key supports in-cluster HTTP Vault as shown; prefer TLS where
available. Pin ECDSA P-256 public keys. Never put private keys or passwords in
configuration. Registry credentials JSON contains username and password only.
Provision zot user prod-signer from kv/registry/prod-zot/prod-signer into the
Kubernetes Secret below through the installation's secret manager. Grant pull
on source repositories and pull/push only on prod-signatures.

Configure auth/k8s-deadnodes-prod/role/cosign-prod-signer with bound namespace
supply-chain, ServiceAccount cosign-prod-signer and audience vault-cosign.
Vault policy needs only update transit/sign/cosign-prod/sha2-256 and read
transit/keys/cosign-prod. Use short leases and Vault audit logging.

Backfill format inside the Flux artifact:

```yaml
digests:
  - ref: registry.deadnodes.internal/deadnodes/app@sha256:REPLACE_WITH_64_HEX
    reason: Reviewed legacy production image
    approved_in: https://forgejo.example/flux/pulls/123
```

Only selected paths are scanned for image fields and repository/tag pairs.
Backfill is read at its configured path even when outside the scanned paths.
Archives are bounded to 64 MiB compressed and extracted content, and links,
traversal, duplicate files and invalid YAML fail closed. No files are extracted
to disk. Each unique digest is processed once, with all observed file paths.

## Kubernetes example

Replace the image with a pinned release digest and provision prod-signer-config
(ConfigMap key config.json) and prod-signer-zot (Secret key credentials.json).

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cosign-prod-signer
  namespace: supply-chain
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: cosign-prod-signer-flux
  namespace: flux-system
rules:
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [gitrepositories]
    resourceNames: [flux-system]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cosign-prod-signer-flux
  namespace: flux-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: cosign-prod-signer-flux
subjects:
  - kind: ServiceAccount
    name: cosign-prod-signer
    namespace: supply-chain
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: prod-signer
  namespace: supply-chain
spec:
  schedule: "* * * * *"
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 1
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      backoffLimit: 0
      activeDeadlineSeconds: 300
      template:
        spec:
          serviceAccountName: cosign-prod-signer
          restartPolicy: Never
          containers:
            - name: signer
              image: ghcr.io/deadnodes/release-control@sha256:REPLACE_WITH_64_HEX
              command: [/usr/local/bin/prod-signer]
              securityContext:
                allowPrivilegeEscalation: false
                readOnlyRootFilesystem: true
                runAsNonRoot: true
                capabilities:
                  drop: [ALL]
              resources:
                requests: {cpu: 50m, memory: 128Mi}
                limits: {cpu: "1", memory: 512Mi}
              volumeMounts:
                - {name: config, mountPath: /etc/prod-signer, readOnly: true}
                - {name: registry, mountPath: /var/run/secrets/prod-signer-zot, readOnly: true}
                - {name: vault-token, mountPath: /var/run/secrets/vault-cosign, readOnly: true}
          volumes:
            - name: config
              configMap: {name: prod-signer-config}
            - name: registry
              secret: {secretName: prod-signer-zot}
            - name: vault-token
              projected:
                sources:
                  - serviceAccountToken:
                      path: token
                      audience: vault-cosign
                      expirationSeconds: 600
```

The normal automounted API token and CA authenticate the minimal stdlib Kubernetes
client. The separately projected vault-cosign token is used only for Vault login.
RBAC grants no list, watch or write access.

Use default-deny ingress/egress NetworkPolicies; allow DNS, the Kubernetes API,
Flux source-controller artifact service, production zot and production Vault
only. Deny dev-to-prod Vault traffic independently. Account for CNI API-service
DNAT behavior and the artifact service's actual namespace/port.

Stdout contains one JSON summary: revision, total, already_signed, signed,
unapproved, errors. Unapproved refs are JSON-escaped on stderr. Remote errors
are redacted; artifact URL query strings and credentials are never logged.
Missing approval exits zero; infrastructure, invalid config/archive or metrics
write failures exit nonzero. Optional metrics_file atomically replaces a
Prometheus textfile containing prod_signer_* counts; mount a writable directory
if enabling it. Live Vault, registry, RBAC and admission-policy acceptance are
operator checks, separate from fake-service tests.
