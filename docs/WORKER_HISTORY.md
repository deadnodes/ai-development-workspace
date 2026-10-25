# Native worker recorded history

`get_worker_history` and `GET /api/worker-history` share the application's
bounded, read-authorized metadata projection over existing worker jobs, attempts,
immutable revisions and frozen operation snapshots. This is native worker
history; GitHub Actions history and UI navigation are separate capabilities.
Reads write no state/events and grant no execution authority.

Supply exact `product_id`, optional `feature_id`, and `collection` (default
`jobs`). Supply `job_id` with `attempts` or `links` to traverse all attempts or
complete frozen Feature/revision associations independently. The Feature filter
selects whole jobs: a selected job's links include its other Features too.
Job cards contain attempt/link counts and recorded source, recipe and operation
references. Failed, expired and cancelled records remain discoverable. Base-only
jobs have zero Feature links; synthetic Integration IDs confer no membership.

`limit` defaults to 25, maximum 100. Follow `pagination.next_cursor` with the
same Product, Feature, job and collection. IDs remain exact. Each page is at
most 32 KiB; byte-limited pages may contain fewer items. Full MCP results are
at most 128 KiB. Live ID-keyset paging is not a snapshot: restart to find
concurrent inserts sorting before the cursor. Cursors are opaque positions,
not authentication or signed verification receipts.

`recorded_revision` means identity/source fields match an immutable captured
revision. `recorded_snapshot_unverified` means job, delivery and frozen parent
ownership, source SHA and revision membership structurally agree. Fingerprints
are not recomputed by this reader. Malformed/mismatched chains have `unknown`
provenance and no asserted Feature links, while remaining in Product history.
No current branch, selection or newest revision repairs attribution. Recorded
`SUCCEEDED` is not verified success, runtime health or deployment acceptance.

PostgreSQL scopes, filters, counts and projects bounded fields before transfer,
without global State reads. bbolt scans its existing JSON blob, skipping unrelated
fields and report/recipe/credential payloads, retaining a bounded page; time cost
may include repeated blob scans for relationships. Unsupported bounded readers
fail closed. No storage migration or additional ledger is introduced.

Existing `get_worker_job` execution/detail protocol is unchanged and remains
an unbounded detail query, not a substitute for this history navigation.
