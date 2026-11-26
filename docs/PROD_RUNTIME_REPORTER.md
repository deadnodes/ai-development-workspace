# Production runtime push reporter

The production cluster runs `rcp-runtime-reporter` as a read-only ServiceAccount.
It lists only configured namespaces and pushes snapshots over HTTPS on the tailnet
from prod to dev. Dev does not connect to prod. Permit the tailnet ACL direction
`tag:rcp-prod-reporter` → `tag:rcp-dev:443`; give the reporter no inbound access or
additional network rights. This code does not install or deploy the reporter.

The same container image contains `/usr/local/bin/release-control` and
`/usr/local/bin/rcp-runtime-reporter`. Override the image **command/entrypoint**
with `/usr/local/bin/rcp-runtime-reporter` (Docker `--entrypoint`, Kubernetes
`command`); supplying args to the server entrypoint does not select the reporter.

## Server configuration

| Variable | Meaning |
| --- | --- |
| `RCP_RUNTIME_REPORTER_TOKEN_FILE` | Secret file read at startup; unset, missing or empty disables reporter authentication. Other read errors fail startup. |
| `RCP_RUNTIME_REPORTER_SCOPE` | One exact `product_id:environment_id:cluster` entry, e.g. `deadnodes:prod:ovh-vpc-k3s-prod`. Empty scope grants nothing. |

The single reporter token is bound to one scope entry. Startup rejects multiple
comma-separated entries, including duplicates. A second cluster requires a future
per-scope token configuration, which is out of scope for this version. Empty
scope still grants no writes.

Also configure `ALLOWED_HOSTS` for the receiving tailnet hostname. Use a dedicated
random token, distinct from `RC_TOKEN` and all worker or agent credentials. The
reporter token works in local and OIDC mode, identifies
`service/prod-runtime-reporter`, and grants only `POST /api/runtime-reports` in
its configured scope. It cannot read reports, call MCP, or run commands.

## Reporter configuration

| Variable | Meaning |
| --- | --- |
| `RCP_URL` | HTTPS origin; defaults to `https://rcp-dev.tail9c31db.ts.net`. |
| `RCP_TOKEN_FILE` | Required mounted bearer-token file; reread every push for rotation. |
| `PRODUCT_ID` | Required target product ID. |
| `ENVIRONMENT_ID` | Target environment ID, default `prod`. |
| `CLUSTER` | Required cluster label, e.g. `ovh-vpc-k3s-prod` (lowercase alphanumeric/hyphens, at most 64 bytes). |
| `NAMESPACES` | Required comma-separated namespace allowlist. No all-namespace fallback. |
| `INTERVAL` | Go duration, default `60s`, minimum `30s`. |

The binary requires in-cluster Kubernetes host/port variables and the projected
ServiceAccount `token` and `ca.crt` under
`/var/run/secrets/kubernetes.io/serviceaccount`. It never loads a developer
kubeconfig. Kubernetes token rotation is honored on every GET. HTTPS redirects
are disabled. Each cycle has a ten-second deadline, writes one log line with
counts/status/success (no token), and retries failures with exponential backoff
from 30 seconds to five minutes. Invalid startup configuration exits nonzero;
cycle failures keep the process alive.

For each allowlisted namespace, bind a Role to the reporter ServiceAccount:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: rcp-runtime-reporter
  namespace: YOUR_NAMESPACE
rules:
  - apiGroups: [apps]
    resources: [deployments, statefulsets, daemonsets]
    verbs: [get, list]
  - apiGroups: ['']
    resources: [pods]
    verbs: [get, list]
```

Use one namespaced RoleBinding per namespace. Grant no watch, mutation, Secrets,
exec, node, or cluster-wide permissions. Pods are associated using the workload
label selector, including selector expressions. Only template containers are
reported; terminating pods are excluded. If observed pods have multiple digests
for a container during rollout, its digest is empty rather than choosing one.
Partial/truncated list failures abort the cycle without sending an incomplete
snapshot.

## Secret provisioning (operator action)

Generate the token into a protected local file, outside the repository. Mount the
same token into the dev server and reporter; never put its contents in YAML,
Git, logs, or command-line literals. For example, an operator can create a Secret
from that file in the reporter's namespace:

```sh
kubectl -n REPORTER_NAMESPACE create secret generic rcp-runtime-reporter-token \
  --from-file=token=/secure/path/rcp-runtime-reporter-token
```

Mount that Secret read-only and set `RCP_TOKEN_FILE` to the mounted file. Provision
the receiving server's token through its own secret manager. Server-side token
rotation requires restarting the receiving process because it reads once at
startup; coordinate the reporter Secret update with that rotation.

## Contract and verification

`POST /api/runtime-reports` accepts `product_id`, `environment_id`, `cluster`,
`reported_at` (RFC3339), and `workloads`. Each workload has `namespace`, `kind`
(Deployment, StatefulSet or DaemonSet), `name`, `generation`,
`observed_generation`, `replicas`, `ready_replicas`, `updated_replicas`,
`available_replicas`, and `containers` (name, image, image_digest, ready_pods,
total_pods). Digests are empty or `sha256:` plus 64 lowercase hex digits.
The body is at most 1 MiB, 1,000 workloads, and 20 containers per workload.
Duplicate workloads/container names, negative counts and timestamps over five
minutes in the future are rejected. Accepted snapshots must advance the
reporter's timestamp and be at least 20 seconds apart by server time; otherwise
HTTP 429 is returned. Clock skew into the future can delay the next acceptance.

Read using an ordinary authorized credential:

```text
get_runtime_report {"product_id":"deadnodes","environment_id":"prod","cluster":"ovh-vpc-k3s-prod"}
```

The same read is available at
`GET /api/runtime-reports?product_id=deadnodes&environment_id=prod` with optional
`cluster`. The response contains `clusters`, each with `reported_at`,
`staleness_seconds`, `stale`, workload count and `workload_records`. Each record
contains `document`, `observed_at`, `changed_at`, and server-clock `absent_since`. Reports older than five minutes
are stale. Missing workloads become `document.absent=true` until expiry or cap
eviction on an accepted report for that cluster. Reappearance clears absence. Unchanged documents retain
`changed_at` while `observed_at` advances.

Snapshots live outside domain.State in dedicated PostgreSQL and SQLite tables, with no State revision/cache updates for snapshot writes. Changed image,
digest, readiness or absence signals produce scoped append-only journal events,
limited to 20 workload events plus one overflow summary per accepted report.
Each exact product/environment/cluster scope entry has a rolling budget of 500
`runtime_report_changed` events per 24 hours, including overflow summaries.
The server reserves this budget atomically with the snapshot, using at most 500
server-time timestamps in `runtime_report_clusters.journal_budget` (equivalent
private snapshot metadata in memory), never in State. Entries expire exactly
24 hours after reservation; process restarts do not reset persisted budgets.
When the budget limits details, `runtime_report_budget_exceeded` is emitted at
most once per rolling hour per scope, carrying `suppressed_changes` and
`suppressed_reports` accumulated since the previous warning. Counts describe
workload details omitted by budget exhaustion, including those represented by
the final overflow summary. No warning is emitted for unchanged reports.
Snapshots continue updating normally and remain the source of truth. Journal
writes follow snapshot commits; a failed journal write can lose reserved events
and does not refund the budget or replay changes. Generation
changes alone do not produce journal events. Observation never authorizes a
production operation or certifies a release.

Runtime reporter scope entries must include an exact cluster; startup rejects malformed
entries or entries without a cluster. Other clusters receive HTTP 403. Accepted
reports are separated by at least 20 seconds across the entire product/environment
scope, including configured clusters sharing that scope (HTTP 429 on violation).
Absent workloads expire seven days after the server first observes absence.
Each cluster stores at most 2000 workloads, evicting oldest absent entries first;
reports that still exceed this cap are rejected atomically with HTTP 413.

`GET /api/runtime-reports` and MCP `get_runtime_report` accept `limit` (default
500, maximum 1000) and `cursor`. Pages count workload records (an empty cluster
counts as one item). Follow `pagination.next_cursor` while `has_more` is true,
keeping product, environment and cluster filters unchanged. Pagination is a live
keyset view, not a frozen snapshot; cluster workload counts describe the full
current snapshot, while `workload_records` contains only the current page.
