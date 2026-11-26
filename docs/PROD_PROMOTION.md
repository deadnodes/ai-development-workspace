# Explicit production promotion

The production flow is **owner go → `request_prod_promotion` → prod promoter verifies,
warms and signs → operator opens the production Flux PR**. The request stores the owner's
verbatim approval and exact image/build identities. Signing happens before the Flux PR. The
promoter does not merge, edit Flux or deploy. Dev workloads receive no prod key, Vault access,
or Kubernetes credentials.

RCP only queues successful, current, main-tagged builds with automatic CI provenance, passing
required CI contexts and no failing recorded security evidence. One invalid build rejects the
entire request. `request_key` replays the same record for identical inputs. The promoter claims
with a ten-minute lease (up to three attempts), independently checks registry and provenance
evidence, then returns per-image outcomes. The promoter renews the claim lease every two minutes during processing. RCP accepts successful outcomes only when the attached
cosign simple-signing payload matches the digest and verifies with its pinned prod public key.

The authenticated RCP API is:

- `POST /api/prod-promotions` — request promotion (`product_id`, `build_ids` with 1–20 IDs,
  `owner_approval`, `request_key`).
- `GET /api/prod-promotions?product_id=…&limit=…` and
  `GET /api/prod-promotions/{id}?product_id=…` — read records.
- `POST /api/prod-promotions/claim` — promoter long poll; JSON `{ "wait": 50 }`, bounded at 55.
- `POST /api/prod-promotions/{id}/result` — promoter reports the claim ID and item outcomes.
- `POST /api/prod-promotions/{id}/heartbeat` — promoter renews the active claim lease by ten minutes.
- `GET /api/public/prod-signatures/sha256:<64 lowercase hex>` — public signature evidence for CI;
  returns only verified signatures attached to succeeded promotions.

The public signature route bypasses OIDC and the local bearer token, but retains host validation;
add the RCP Kubernetes Service hostname used by CI to `ALLOWED_HOSTS`.

MCP provides `request_prod_promotion`, `get_prod_promotion`, and `list_prod_promotions`. The
request tool is the only way to obtain cosign-prod signatures. It requires the owner's explicit go
quoted verbatim in `owner_approval`, and does not merge or deploy anything. The UI shows recent
promotion and per-image status in Product configuration → Releases.

## RCP configuration

- `RCP_PROD_REGISTRY_HOST` sets the promoted registry host; default
  `registry.deadnodes.internal`.
- `RCP_PROD_PUBLIC_KEY_FILE` points to the pinned ECDSA P-256 prod public-key PEM. If absent,
  promoter `SIGNED`/`ALREADY_SIGNED` results are refused and the public signature endpoint has no
  data to return.
- `RCP_PROD_PROMOTER_JWKS_FILE` points to static public JWKS from the prod cluster's ServiceAccount
  issuer. Unset/empty disables promoter authentication.
- `RCP_PROD_PROMOTER_ISSUER` is the exact JWT `iss` value. Set it to the issuer reported by
  `kubectl get --raw /.well-known/openid-configuration` on the prod cluster.
- `RCP_PROD_PROMOTER_AUDIENCE` defaults to `rcp-prod-promoter`.
- `RCP_PROD_PROMOTER_SUBJECT` defaults to
  `system:serviceaccount:supply-chain:cosign-prod-signer`.
- `RCP_PROD_PROMOTER_SCOPE` is one `product_id:environment_id` pair, normally
  `deadnodes:prod`.

RCP pins the static JWKS at startup. To obtain it, run on the prod cluster:

```sh
kubectl get --raw /openid/v1/jwks > prod-serviceaccount-jwks.json
kubectl get --raw /.well-known/openid-configuration
```

Copy the public JWKS into the dev-side RCP configuration through its normal configuration channel;
it contains public keys, not a bearer token. When prod rotates its ServiceAccount signing key, the
promoter's new JWT will receive HTTP 401 until the JWKS file is refreshed and RCP reloads/restarts.
Keep at least the active verification key available during planned rotations.

## Prod deployment shape

Run `prod-signer -mode promote -config /etc/prod-signer/config.json` in the production
`supply-chain` namespace as ServiceAccount `cosign-prod-signer`. Retain its current projected
`vault-cosign` token and registry credentials. Add this separate, audience-bound RCP token volume to
the pod (Kubernetes rotates the projected token; prod-signer rereads the file on every RCP call):

```yaml
spec:
  serviceAccountName: cosign-prod-signer
  hostAliases:
    - ip: 10.43.200.12
      hostnames: [rcp-dev.tail9c31db.ts.net]
  containers:
    - name: prod-signer
      args: ["-mode", "promote", "-config", "/etc/prod-signer/config.json"]
      volumeMounts:
        - name: rcp-token
          mountPath: /var/run/secrets/rcp
          readOnly: true
  volumes:
    - name: rcp-token
      projected:
        sources:
          - serviceAccountToken:
              path: token
              audience: rcp-prod-promoter
              expirationSeconds: 600
```

Configure `rcp.url` as HTTPS, `rcp.token_file` as `/var/run/secrets/rcp/token`, and set
`product_id` / `environment_id` to the scope allowed by RCP. The tailnet ACL should allow the prod
signer tag to reach only `rcp-dev:443`; prod can reach RCP over the tailnet, and Dev cannot reach
production. Do not mount a prod ServiceAccount credential, Vault token or signing key in Dev.

## Signer configuration and behavior

Promote-mode config keeps the existing strict registry, Vault, dev key, prod key, builder IDs,
registry rewrite and own-image-prefix settings, and adds:

```json
"rcp": {
  "url": "https://rcp-dev.tail9c31db.ts.net",
  "token_file": "/var/run/secrets/rcp/token",
  "product_id": "deadnodes",
  "environment_id": "prod"
}
```

`flux_artifact`, `paths` and `backfill_file` are not required in promote mode. Accepted production
refs must be under `registry.deadnodes.internal/deadnodes/` and configured own prefixes. Zot warming
is retried for up to `warm_timeout_seconds` (default 900; max 900), with a per-request
`registry.request_timeout_seconds` (default 300; max 900). Warming also fetches dev `.sig` and
`.att` referrer manifests and HEADs their blobs. Infrastructure reasons are `WARM_TIMEOUT`,
`WARM_TAG_MISMATCH`, `REGISTRY_UNAVAILABLE`, `DEV_EVIDENCE_UNAVAILABLE`, `VAULT_SIGN_FAILED`,
`SIGNATURE_PUBLISH_FAILED`, and `SIGNATURE_READBACK_FAILED`; deterministic REJECTED reasons are
unchanged. Dev signature, builder ID, `main-` provenance, successful
required contexts, build ID, source SHA and image revision are independently rechecked. The signer
records only a SHA-256 of the approval text in its prod approval predicate. A failed image is
reported independently; any non-success item leaves the promotion FAILED and requires a new request.

The existing Flux one-shot mode remains the default for now. Its post-merge CronJob is planned for
removal in a separate change after this pre-Flux flow is deployed and accepted.
