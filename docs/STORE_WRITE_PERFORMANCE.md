# PostgreSQL write performance

This change removes full PostgreSQL state reloads and full-state JSON diffs from
ordinary feature, integration and memory commands. It does not deploy or restart
an installation.

## Transaction and cache contract

The database revision row still serializes writers across processes. Each update
locks that row and reads its revision. A matching process-local cache supplies
committed records; a different revision reloads records and the write provenance
projection. Failed callbacks, validation, SQL statements and commits never publish
mutated cache contents. Cache publication follows commit. An uncertain commit is
resolved by the next revision check.

Both reads and updates reserve a pool connection before taking the cache mutex.
No pool acquisition may hold that mutex: otherwise a reader owning the last
connection can wait for a writer that owns the mutex and is waiting for the pool.
The PostgreSQL regression forces this interleaving with `pool_max_conns=1` and
requires concurrent Read/Update to finish successfully within ten seconds.

Application commands declare trusted mutation scopes. Feature/integration
commands copy their mutable collections; memory creation shares immutable old
memory values and detaches new documents. Derived gates and parent feature
reactivation are included. Scope tests check actual command mutations, including
nested append-only values. Changed typed records are compared before encoding;
only changed IDs are marshaled and upserted, in bounded SQL batches. Matching JSON
does not rewrite positions. Omitted historical records remain retained unless the
existing product-deletion predicate permits deletion. Cache order follows stored
positions, including ties, omissions and gaps. Persisted JSON normalization and
empty collection shapes agree with a fresh database read.

Unknown callbacks retain a fully isolated `Store.Update` fallback. It copies and
compares all record collections in memory, but does not reload the database on a
revision hit or marshal unchanged records. This fallback is still O(record state)
in CPU/allocation; the sub-second target is verified for the measured ordinary command paths,
not every provider orchestration, restore or arbitrary callback.

Full validation runs on cache loads and unscoped mutations. Ordinary deltas check
changed statuses and the ID/audit edges that can affect otherwise unchanged pin
and registry-inventory invariants. Commands changing their reference collections
retain complete validation. Empty-dependent validator fast paths have the same
acceptance behavior without building unused indexes.

## Journal and authorization

Writes never load the complete journal. The incrementally maintained write index
contains these event families:

| Consumer | Indexed evidence |
| --- | --- |
| Source-retention ancestry | `assemble_environment`, `reconcile_composition`, `update_environment_feature`, `restore_backup`, preserving relative append order and duplicate evidence |
| Worker failure admission | All `worker_report` events, retaining malformed/duplicate reports for rejection |
| Artifact pins and retention fingerprints | `pin_artifact`, `release_artifact_pin`, including mismatched command actions; reserved record/event ID collisions are checked explicitly |
| Creation identity | Indexed records/events ID lookup in the same write transaction |
| Authenticated commands | Indexed command-reference closure, including audit IDs and the existing environment/composition/candidate/operation/parent edges |

Authorization remains in the application layer. Preflight reads select the
reference closure rather than full State; admission is rechecked inside the
revision-locked transaction. Production binding metadata and cross-product event
references remain enforced. Only the referenced closure is JSON-projected for
policy evaluation. Tests cover direct and transitive foreign audit references,
cycles, duplicate identities and projection parity for provenance consumers.

History, HTTP full snapshots, backup/export, local Git plan lookup and full
feature context continue to read the complete journal outside the global writer
lock. Existing bounded navigation/context/history APIs keep their SQL bounds.
`Store.Read` reuses revision-matched records and returns a detached snapshot; a
successful cold read can also warm the record cache. Full snapshots still have
O(journal) output/decode cost and are not sub-second endpoints.

Migration 005 adds a partial write-provenance sequence index and a
product/feature/sequence history index. There is one current schema.

The bbolt backend is unchanged: its isolated, transactional JSON blob remains
correct, including product deletion and rollback, but reads and writes retain
O(blob) serialization costs. It does not receive the PostgreSQL cache or scoped
mutation implementation.

## Reproduction

The opt-in test refuses databases other than numeric `rcp_perf_N` names. It selects
an existing feature, runs 20 commands through `application.Service.Execute`, and
reports nearest-rank p50/p95, maximum, first/cold write and warm maximum. Timing
includes application policy and Store.Update, not HTTP/MCP network transport.

For every run, create a new database from the read-only fixture, run the test on
that copy, then drop the copy:

```sh
psql "$RCP_PERF_ADMIN_URL" -c 'CREATE DATABASE rcp_perf_10 TEMPLATE rcp_snapshot'
RCP_PERF_DATABASE_URL="$RCP_PERF_COPY_URL" \
  go test ./internal/persistence -run '^TestSnapshotWritePerformance$' -v -timeout 60m
psql "$RCP_PERF_ADMIN_URL" -c 'DROP DATABASE rcp_perf_10'
```

Set `RCP_PERF_MIXED=1` for alternating progress, explicit-ID integration creation
and feature update commands. Set `RCP_PERF_AUTHENTICATED=1` to exercise the same
path with a non-admin agent principal, including authorization preflight and
transactional authorization. With `RCP_PERF_DATABASE_URL` unset, the test skips.

## Measurements and verification

Fixture: PostgreSQL on localhost:55438, `rcp_snapshot`, 284,658 records and 177,794
events. The source fixture and live service were not modified. Each run used its
own `CREATE DATABASE ... TEMPLATE rcp_snapshot` copy. The exact-main baseline
uses detached main `ec34d6edb943cf0399bea0aa9d1c8b4f2274624b` with only the opt-in
test copied into that checkout. Runs share a host with test workloads; each copy
has one benchmark writer.

Implementation commits: `041f951` and `2e03fb3`; fixture-only follow-up: `65e85c5`.
Historical measurements below use `65e85c5`. The lock-order regression follow-up
was measured from `32d84d0` plus the connection-before-cache-mutex fix.

| Run (20 writes) | p50 | p95 | Maximum | First/cold | Warm maximum |
| --- | ---: | ---: | ---: | ---: | ---: |
| Main, progress | 66.237 s | 104.049 s | 126.494 s | 104.049 s | 126.494 s |
| After, progress | 0.112 s | 0.371 s | 18.140 s | 18.140 s | 0.371 s |
| After, authenticated mixed | 0.392 s | 0.544 s | 14.427 s | 14.427 s | 0.544 s |
| After lock-order fix, progress | 0.072 s | 0.127 s | 8.066 s | 8.066 s | 0.127 s |

The lock-order follow-up used one fresh `rcp_perf_20261113` template copy on
2026-11-13, with no verification checks overlapping the measurement. All 19 warm
writes completed below one second; the copy was dropped after the run. The
regression passed against the dedicated `rcp_perf2_tests` database with one pool
connection, and failed at its ten-second deadline with the original lock order.
`go test -race ./internal/persistence/ -timeout 30m` passed in 138.597 seconds
with PostgreSQL enabled. Formatting, `go vet ./...`, server build and JavaScript
syntax checks also passed. No live RCP was contacted for this follow-up.

The like-for-like progress p95 improved about 280 times. All 19 warm writes in
both final after-runs completed below one second. Cold reloads are explicitly
outside that warm latency result; an external writer's revision change also
requires a reload. The mixed run contains seven progress writes, seven explicit-ID
integration creations and six feature updates. Its principal has read/write
permissions and wildcard product access, but is not an instance admin.

A separate authenticated trial overlapping vet/build measured p95 **1.031 s**
(cold 19.377 s). It is retained in the sample file rather than hidden. The final
serial runs above did not overlap checks started by this task; the host can still
have unrelated workloads. These are measured results, not a latency guarantee
under arbitrary contention.

Exact nanosecond samples and percentile inputs are in
[STORE_WRITE_PERFORMANCE.json](STORE_WRITE_PERFORMANCE.json). Final copies
`rcp_perf_6`, `rcp_perf_12`, `rcp_perf_13` and `rcp_perf_14`, all intermediate performance copies,
and the detached baseline worktree were removed after measurement.

### Verification

| Check | Result |
| --- | --- |
| `gofmt` on all changed Go files | Passed; no remaining formatting output |
| `go vet ./...` | Passed |
| `node --check web/static/app.js` | Passed |
| `go build ./cmd/server` | Passed; generated binary removed |
| Race-enabled non-application packages | 21 packages with tests passed; 7 packages have no tests |
| Race-enabled application coverage | All 307 top-level tests verified across serial coverage, four disjoint partitions and targeted reruns |
| Optional tests skipped in the default sweep | 25; exact names and reasons in the verification JSON |
| Snapshot performance test | Default skip, then separately executed on disposable copies as reported above |

The full `go test -race -p 2 ./... -timeout 60m` command was run during development.
The serial application suite was too slow for a practical single run on this host
(only 91 of its original 306 test roots had completed after more than 20 minutes),
so its remaining 215 roots were split into four disjoint sets of 54/54/54/53.
The additional explicit-ID feature replay regression makes 307 application roots.
[STORE_WRITE_TEST_PARTITIONS.json](STORE_WRITE_TEST_PARTITIONS.json) lists every
root and the exact groups. Non-application packages were verified using:

```sh
TEST_DATABASE_URL="$RCP_TEST_COPY_URL" go test -race -p 2 \
  $(go list ./... | rg -v '^releasecontrol/internal/application$') \
  -timeout 60m -json
```

Application partitions used `go test -race ./internal/application -run <anchored
root-name alternation> -timeout 60m -json`. Fixture reset failures were corrected
by preserving the callback's supplied append-only event index while resetting
records; full reader journals must not replace a write index. Their targeted
reruns passed. The existing large PostgreSQL history fixture now has a five-minute
setup/verification context instead of 90 seconds under race instrumentation.

Two recovery tests hit wall-clock budgets under concurrent race load. The
25-second production worker-call deadline test passed when rerun serially, with
that deadline unchanged. The long retained-root settlement test spent more than
five seconds in bbolt fixture setup even in isolation. Only that test opts into a
30-second native-worker fixture lease; other expiry/fence tests keep five seconds.
All eight local/PostgreSQL settlement modes passed across the original execution
and race-enabled targeted reruns. No production timeout or lease policy changed.

The database URL supplied to this environment used a name without the `_tests`
suffix required by several existing integration guards. Verification therefore
used the newly created disposable `rcp_store_write_perf_tests` on localhost:55437,
with isolated schemas, and dropped that database afterward. PostgreSQL integration
tests were actually executed; they were not claimed from a skipped run.

[STORE_WRITE_VERIFICATION.json](STORE_WRITE_VERIFICATION.json) records package
results, exact optional skips and final coverage. The optional owned-Docker,
BuildKit, live-provider and large-UI-read fixtures were not enabled. UI source was
unchanged, so browser validation was not required. No live RCP progress, release,
installation update, image publication, container restart or push was performed.
