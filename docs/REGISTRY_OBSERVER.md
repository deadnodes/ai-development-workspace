# Read-only registry observer

`bin/rcp-registry-observer run-once OPERATOR_CONFIG.json` claims one
`registry_observe` job through the existing worker protocol. It reads the exact
configured registry repository, reports a bounded OCI graph and exits. It never
builds application code, publishes an image or deletes registry content.

## Operator configuration

The control plane's private worker configuration has an `observations` array of
`workerconfig.RegistryObserveRecipe` values. An observer-only installation may
use an empty `recipes` array; it does not need a fake source recipe.

Each observation recipe contains `id`, `revision`, `hash`, `product_id`,
`repository_id`, `component_id`, `origin`, `repository`, `limits` and `pull`.
Compute its hash with `workerconfig.ObservationHash` after setting the other
fields. The origin and repository are exact operator authority, not request
parameters. The limits bound response bytes, total bytes, nodes, edges, depth,
pages, requests and elapsed time.

`pull` uses the existing registry read configuration: `ca_file`, either
`username_env` plus `password_env` or `bearer_env`, and the isolated-test-only
`fixture_loopback_http` option. Normal origins require HTTPS. Requests use the
configured origin with certificate verification, without environment proxies,
redirects, challenge delegation or descriptor-URL fetching.

The runner configuration contains `endpoint`, `token_env`, `config_revision`,
`state_dir` and the identical `recipe`. `state_dir` must already exist as a
private, canonical absolute directory with mode `0700`. The runner retains its
lease/report journal and completed-job records there. Restart uses the retained
observation window and report identity, not a newly dated copy of old evidence.

Grant its expiring credential the exact Product and owning repository with
`registry_observe` capability. Keep the observer principal and token separate
from source/build, publishing and control credentials. Registry credential values
remain local environment values; neither RCP state nor reports receive them.

## Request and readback

Use `refresh_registry_inventory` through the ordinary authenticated command
interface with Product scope and the exact component, recipe/configuration pins,
request key and deadline described in [artifact retention](ARTIFACT_RETENTION.md).
The server freezes known image roots; callers cannot choose arbitrary targets.
Inspect the resulting Operation and the `registry` summaries in
`get_artifact_retention`. Reading that query does not start another scan.
Its configured-scope summary exposes the nonsecret recipe ID, revision, hash and
configuration revision needed for a refresh request.

The observer verifies supported OCI/Docker indexes and manifests, configs,
layers, subjects and referrers across platforms. Discovery and byte failures
produce conservative partial or unknown evidence. The retained report is
`WORKER_ATTESTED_REGISTRY_READ`; it does not claim independent server byte
verification. A successfully completed job is not proof of complete inventory.

Known-root and tag discovery cannot enumerate all untagged manifests or orphaned
blobs. Full backend inventory, a consistency barrier, deletion fencing and
destructive approval remain separate requirements. `global_inventory_complete`
and retention `deletion_allowed` remain false.
