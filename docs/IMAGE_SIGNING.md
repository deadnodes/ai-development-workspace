# Image signing

RCP can sign registry-verified successful Forgejo Builds using Vault Transit.
This is optional operator authority in `RCP_BUILDS_FILE`; omission of `signing`
keeps signing disabled. Signing never changes Build.status or authorizes a release.
Signing configuration is excluded from build authority hashes. Restart to reload.

RCP signs only within its own contour, using cosign-dev. Never grant the dev
server access to production Vault or production signing credentials. Production
approval belongs to the separate [prod-signer](PROD_SIGNER.md) in the prod cluster.
The explicit request path and planned prod-side promotion protocol are described in
[PROD_PROMOTION.md](PROD_PROMOTION.md).
The generic multi-key adapter does not establish a cross-contour trust boundary.

## Configuration

See `examples/forgejo-builds.json` for a complete configuration with public-only
example keys. Replace the PEMs with your own pinned Transit public keys before use.

- `builder_id`: HTTPS identifier for this RCP builder (paths are allowed).
- `keys`: unique `id`, `vault_addr` origin, `transit_mount`, `key_name`,
  `public_key_pem` (SPKI ECDSA P-256), `auth` and `allow_backfill`.
  HTTP Vault requires explicit `insecure_http:true` on that key.
- `auth`: `method` (`kubernetes` or `jwt`), `mount`, `role`, absolute `token_file`.
  Both methods exchange the projected JWT for a Vault client token. Tokens are
  bounded to 64 KiB, reread on login and cached for 80% of the returned lease.
- `rules`: `key_id`, nonempty `tag_prefixes`, optional `require_default_branch`
  and `require_status_contexts`. Default-branch signing requires recorded main-tag
  evidence and a default/unspecified build branch. CI uses the existing trusted
  producer/status-context verdict and, when configured, PR-tree equivalence gate.
- `registry_credentials`: map of existing registry ID to a single JSON secret
  filename in `RCP_SECRET_DIR`, containing `username` and `password`. The shared
  secret-file reader confines symlinks. No credentials enter domain records.
- `require_signature_for_deploy`: default false. When true every deployed image
  needs durable SIGNED evidence and fresh registry verification under an allowed
  pinned key. Failure is `SIGNATURE_MISSING`. When false, a fresh check still
  records `signature_verified` without blocking delivery.
- `deploy_keys`: map of configured environment IDs to allowed signing key IDs.
- `backfill_repositories`: optional repository prefixes, ending in `/`.

Basic registry authentication and same-origin Bearer token challenges are
supported. Challenges and upload locations cannot redirect credentials to a
foreign origin. All clients reject redirects, enforce 30-second HTTP timeouts,
bound responses to 4 MiB and return fixed redacted errors.

## Vault requirements

Create an ECDSA P-256 Transit key. Export only its public key and pin that PEM in
RCP. Give each authentication role only the required key permissions:

```hcl
path "transit/sign/cosign-dev/sha2-256" {
  capabilities = ["update"]
}
path "transit/keys/cosign-dev" {
  capabilities = ["read"]
}
```

Bind Kubernetes/JWT roles to the expected ServiceAccount identity, audience and
cluster issuer, with short leases. Mount a projected ServiceAccount token at the
configured absolute path. Do not use a static Vault root token. Enable Vault audit
logging for Transit sign calls; audit access and retention are operator concerns.
Vault owns the private key; RCP sends a SHA-256 digest with `prehashed:true` and
`marshaling_algorithm:"asn1"`. Each response records the Vault key version.
RCP compares the latest Vault public key against the pinned key and verifies each
returned DER signature locally. Mismatch fails with `SIGNING_KEY_MISMATCH` before
publishing that artifact. Rotation requires updating the pin deliberately.

## Flow and format

Only builds transitioning to SUCCEEDED with signing configured become PENDING;
startup never backfills historical successes. The existing 15-second worker signs
eligible keys, retains per-image/key evidence and appends `build.signed` or
`build.signing_failed` audit events. Pending CI defers; failed CI skips that key
with `CI_NOT_GREEN`. Transient failures use exponential retries capped at 30
minutes, after which an explicit request with a fresh request_key can retry.
Build signing status is PENDING, SIGNED, PARTIAL, FAILED or SKIPPED; disabled is
an empty string. Each signature records key ID, key version, SHA-256 DER SPKI
fingerprint, digest, signature tag, attestation predicate types and signing time.

The signature is attached to the exact top-level BuildImage digest, including an
index digest. It uses the classic cosign v2 `sha256-<hex>.sig` tag in the same
repository, an OCI image manifest/config and one uncompressed simple-signing JSON
layer per key. The DER ECDSA signature is a base64 layer annotation. The payload
contains the exact repository/digest and RCP build/source/key identity.

Provenance uses `sha256-<hex>.att`, DSSE `application/vnd.in-toto+json`, an in-toto
v0.1 statement and `https://slsa.dev/provenance/v1`. The predicate contains the
Forgejo build definition, workflow/dispatch/source/tag inputs, RCP profile/build
IDs, resolved Git source, configured builder, run times and CI-context byproducts.
DSSE signs the byte-length-prefixed PAE, not the base64 representation.

Existing foreign layers are retained. Payload/key identity suppresses duplicate
signatures; statement identity plus verification suppresses duplicate randomized
DSSE signatures. Config diff_ids track every layer digest. A single installation
serializes appends; OCI mutable tags have no portable compare-and-swap. Operators
must coordinate concurrent external writers to the same tags to prevent lost
updates. Layer count is bounded to 256 and existing malformed artifacts fail closed.
`imagesigning.Predicate` is the extension point for a later CycloneDX predicate;
fetching SBOM referrers is not implemented.

## Explicit dev backfill

MCP `sign_artifact` or `POST /api/signatures` takes:

```json
{
  "product_id": "example",
  "profile_id": "service",
  "image_id": "web",
  "image_ref": "registry.example.invalid/team/service@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "key_ids": ["dev"],
  "reason": "Operator approved legacy artifact provenance",
  "request_key": "backfill-001"
}
```

Requires Product write permission, a configured image ID, an exact digest in that
profile's registry, and either the configured image repository or an allowlisted
backfill prefix. Dev keys only: each key must allow backfill or be a configured rule key.
HEAD verifies existence, then digest-checked image/index/config reads extract a
revision label if present. Backfill provenance uses buildType
`https://deadnodes.dev/rcp/backfill/v1`, reason, actor and observed revision.
A persisted handle is returned before signing I/O. Receipts bind normalized input;
identical request_key replays return the historical handle, conflicting reuse
fails. Use a fresh key for an explicit retry. MCP `list_signatures` / GET
`/api/signatures?product_id=…&digest=…&limit=25&cursor=…` lists these handles.
Automatic Build evidence remains on `get_build`. Restored pending work is stopped
and needs an explicit new request, preserving already recorded evidence.

## Offline verification and trust

```sh
cosign verify --key pub.pem --insecure-ignore-tlog=true registry.example.invalid/team/service@sha256:DIGEST
cosign verify-attestation --key pub.pem --insecure-ignore-tlog=true --type slsaprovenance1 registry.example.invalid/team/service@sha256:DIGEST
```

For a deliberately insecure local registry add `--allow-http-registry`.
Kyverno can verify against the same PEM offline; no Rekor or Fulcio is used.
A signature proves possession of the pinned key over these bytes. It does not
prove safe source, correct CI, runtime health, Flux trust or production approval.
Compromised RCP/Vault authorization can sign malicious content. Protect operator
configuration, projected tokens, Vault policy and registry credentials separately.
Registry deletion can remove signatures; the deploy gate rechecks current bytes.
