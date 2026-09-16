# Feature delivery graph

Open any Feature in the web UI. Delivery graph is the default view.

- Each repository lane connects recorded source branches, linked PRs and target branches. Select a repository to see individual PR directions and merge SHAs; select a PR for timestamps, integration links and the original evidence record.
- Filter by repository or PR state. Source slices show immutable integration revisions and environment compositions. Environments shows feature-scoped deployment operations, build/image evidence and runtime observations.
- Work & history retains the existing editing, verification, decisions and handoff interface.
- Graph state refreshes with the workspace's existing polling. Refresh graph reloads stored evidence; it does not contact GitHub or mutate remote branches.

The same application projection is available through `GET /api/features/{feature_id}/graph` and MCP `get_feature_graph({"feature_id":"..."})`.

## Evidence semantics

PR arrows describe source-to-target PR direction, not a reconstructed Git commit DAG. SHA lists alone do not prove ancestry or deployment. Historical PR metadata may be enriched from a matching integration progress record; the record ID and observation timestamp remain visible.

Imported production snapshots remain explicitly **reported / not live**. GitOps applied is desired state, while runtime observations carry their own timestamp, digest and health. Missing evidence is unknown. No release, provider write or runtime observation is created by viewing the graph.

The graph is scoped to the Feature's Product and related integrations. Multi-repository operations retain source repository identities. Source compositions remain immutable selections, distinct from generated branch names.

Run `node web/graph-check.mjs` for browser DOM interaction/security/race tests, and `go test ./internal/application ./internal/transport` for projection and transport behavior. UI acceptance also requires checking the real page in the browser.
