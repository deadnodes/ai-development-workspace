# Repository purposes and library components

A registered repository is a physical Git identity shared across the instance. Its role belongs to each Product attachment: the same repository can be used differently by different Products. A Component is a deployable application or a reusable library at a repository path; the compatibility API name remains `Application` / `create_application`.

| Repository role | Meaning |
| --- | --- |
| APPLICATION | Deployable application source |
| LIBRARY | Reusable package/library source |
| MIXED | Application and library source, commonly a monorepo |
| GITOPS | Desired environment configuration |

Roles, component kinds and publication formats are finite domain-owned catalogs. `GET /api/statuses` and MCP `get_status_schema` include `repository_roles`, `component_kinds`, and `publication_formats`. Both repository creation/import schemas use the same values. New attachments default to APPLICATION. SOURCE is not a valid purpose.

Create components with `create_application`, `product_id`, and `data: {name, repository_id, path?, kind}`. Kind is APPLICATION or LIBRARY; new components default to APPLICATION. An application can represent one Kubernetes Deployment or a chart containing several resources: the Control Plane tracks the configured delivery unit, not a mandatory one-resource mapping. Desired GitOps configuration is separate from reconciliation and runtime health.

Libraries have publication targets and immutable package artifact records. They are excluded from environment composition, image build configuration and deployment selectors. OCI can be a library publication format; that does not turn the library into an environment application.

Use shared MCP `execute` or HTTP `/api/commands`:

```json
{"action":"configure_publication","actor":"agent/packages","product_id":"PRODUCT_ID","data":{"application_id":"LIBRARY_ID","format":"npm","registry_url":"https://registry.npmjs.org","package_name":"@example/sdk"}}
```

Supported formats are npm, pypi, nuget, maven, oci and generic. Record an externally published artifact with exact source/version/checksum provenance:

```json
{"action":"record_package_artifact","actor":"agent/packages","product_id":"PRODUCT_ID","integration_id":"INTEGRATION_ID","data":{"application_id":"LIBRARY_ID","publication_target_id":"TARGET_ID","source_commit":"FULL_SOURCE_SHA","version":"1.2.3","checksum":"sha256:ACTUAL_PACKAGE_HASH","uri":"https://registry.example/package/1.2.3","build_url":"https://ci.example/build/123"}}
```

Integration and build URL are optional. These commands persist caller-attested intent/evidence. They do not run a publisher, authenticate to a package registry, verify package availability, or deploy a library to Kubernetes. The UI labels that limitation and preserves the recording actor/time. No registry credentials should be placed in URLs or artifact metadata.

To change an attachment purpose, use `classify_repository` with `id` set to its Product repository ID and `data: {role: "APPLICATION"}` (or another catalog role). The server checks existing component/mapping compatibility and records the change in audit history. The UI exposes “Classify …” beside Product repositories. This does not reclassify the shared physical registry identity or silently change an existing component’s kind.
