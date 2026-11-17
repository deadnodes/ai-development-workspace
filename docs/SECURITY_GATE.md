# DeadNodes build security gate — evidence contract (v1)

Producer: `dn-image-scan gate` (ansible role `trivy_scan`, build hosts).
Consumer: Release Control Plane (signing, deploy gate, prod-signer).

For every built image digest the gate pushes TWO OCI 1.1 referrers into the same
zot repository as the image (subject = the image manifest/index digest):

## 1. SBOM referrer
- OCI image manifest (`application/vnd.oci.image.manifest.v1+json`)
- `artifactType`: `application/vnd.cyclonedx+json`
- `config`: empty descriptor (`application/vnd.oci.empty.v1+json`, digest of `{}`, size 2)
- one layer, mediaType `application/vnd.cyclonedx+json`: Trivy CycloneDX JSON SBOM of the image
- `subject`: {mediaType, digest, size} of the image as served by zot
- annotations: `org.opencontainers.image.created` (RFC3339 UTC),
  `dev.deadnodes.security.source-sha` (40-char sha)

## 2. Security-gate summary referrer
- OCI image manifest, `artifactType`: `application/vnd.deadnodes.security-gate.v1+json`
- empty config, one layer mediaType `application/vnd.deadnodes.security-gate.v1+json` (the summary JSON below, <= 1 MiB)
- same `subject`
- annotations: `org.opencontainers.image.created`, `dev.deadnodes.security.source-sha`,
  `dev.deadnodes.security.verdict` (PASS|FAIL|ERROR)

Summary JSON:
```json
{
  "schema": "deadnodes.security-gate/v1",
  "verdict": "PASS",                       // PASS | FAIL | ERROR
  "reasons": [],                           // human-readable, no secret values
  "image": "192.168.32.242:5000/deadnodes/poster:main-1234abcd@sha256:…",
  "digest": "sha256:…",                    // == subject digest
  "sha": "<40-char source sha>",
  "repository": "deadnodes/poster",
  "created_at": "2026-11-18T10:00:00Z",
  "policy": {"block": ["CRITICAL", "HIGH_FIXABLE", "SECRET"], "max_ignore_days": 180},
  "gitleaks": {"version": "8.30.1", "status": "OK", "findings": 0, "allowlisted": 0,
               "rules": [{"rule": "aws-access-token", "file": "x.py", "line": 3, "fingerprint": "x.py:aws-access-token:3"}]},
  "trivy": {"version": "0.76.1", "status": "OK", "db": {"UpdatedAt": "…", "DownloadedAt": "…"},
            "critical": 0, "high": 0, "high_fixable": 0, "blocking": 0,
            "blocking_ids": [], "ignored": ["CVE-…"]},
  "sbom": {"format": "cyclonedx-json", "digest": "sha256:<SBOM referrer manifest digest>",
           "layer_digest": "sha256:…", "components": 123}
}
```

Verdict rules:
- FAIL if gitleaks has any non-allowlisted finding, or trivy has any non-ignored
  CRITICAL vulnerability, or any non-ignored HIGH vulnerability with a FixedVersion.
- ERROR if a scanner failed, the vulnerability DB is missing, or the SBOM could not be produced/pushed.
- PASS otherwise. `blocking` = number of blocking vulnerability findings.

Allowlists (read from the checked-out source at the build SHA, reviewed via PR):
- `.trivyignore`: existing policy — `ID exp:YYYY-MM-DD`, preceded by `# reason:`; expiry required, <= 180 days.
- `.gitleaksignore`: gitleaks fingerprint lines (`path:rule:line`), each preceded by one or more
  non-empty `# reason:` comments; optional ` exp:YYYY-MM-DD` suffix (if present must be in the future,
  <= 180 days). Invalid lines are dropped with warnings. Repository `.gitleaks.toml` is NOT used;
  the gate uses gitleaks default rules plus a host-managed config.

RCP consumption:
- reads `GET /v2/<repo>/referrers/<digest>?artifactType=application/vnd.deadnodes.security-gate.v1+json`,
  newest by `org.opencontainers.image.created`; checks schema, digest == image digest, sha == build
  source sha, SBOM referrer with `sbom.digest` exists for the same subject.
- profile `require_security_gate: true` → signing needs every image PASS; provenance gets a
  `security_scan` byproduct; prod-signer (`require_security_scan`) refuses without byproduct PASS.

## RCP behaviour

RCP reads the OCI referrers above and does not parse build logs. A gated profile records
`security_status` as `PENDING`, `PASS`, `FAIL`, `ERROR`, or `MISSING`; profiles without
the gate leave it empty. Missing evidence remains pending for 30 minutes after build
completion, then becomes `MISSING`. Registry errors remain pending for retry. A source SHA
mismatch or malformed evidence is `ERROR`.

Signing waits while status is empty or `PENDING`. `FAIL` and `ERROR` produce skipped
signature entries with `SECURITY_GATE_FAILED`; `MISSING` produces
`SECURITY_GATE_MISSING`. Deploy requires `PASS` and otherwise fails with
`SECURITY_GATE_NOT_PASSED`. Passing signatures carry a per-image provenance byproduct
named `security_scan` with the summary and SBOM digests and scan counts. Prod-signer can
set `require_security_scan: true` to require that byproduct to report `PASS`, zero
blocking vulnerabilities, and zero Gitleaks findings.

Enable gradually per profile in the Flux `builds` ConfigMap by setting
`require_security_gate: true` on the selected profile. Confirm the build host publishes
both referrers before enabling the profile; expand to other profiles after observing
their evidence and statuses.
