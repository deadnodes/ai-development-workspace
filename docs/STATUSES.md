# Controlled statuses

Statuses are a finite domain contract, not arbitrary labels or a configurable workflow engine. The application validates values and transitions for both humans and agents; HTTP, MCP and the UI share that domain layer.

Discover the complete current catalog and transition graph with `GET /api/statuses` or MCP `get_status_schema {}`. The response contains `catalog` and `transitions`. `/api/schema` and the MCP `execute` schema also constrain client-writable status values. The UI loads the domain catalog and only offers the current state and permitted next states.

| Entity | Statuses |
| --- | --- |
| Feature | planned, active, blocked, completed, archived |
| Integration | planned, working, implemented, verifying, ready, released |

A Feature’s completed state describes implementation scope; it does not imply production delivery. An Integration’s ready state means readiness checks passed; it does not mean released. `start_integration`, `transition_integration` and `complete_integration` retain their application guards. Clients cannot assign released through plan edits or generic transitions: successful release execution records it. The graph describes potential transitions, not permission to bypass dependencies, blockers or verification.

Check outcomes, findings, operations, deployment states and other server-owned states use the same catalog endpoint. Record their dedicated engineering actions and evidence instead of attempting direct status edits. Existing persisted data is validated by the domain; unknown values require an explicit audited repair rather than silent acceptance as a new lifecycle state.

## Command transition rules

Same-state requests are idempotent. Unknown spellings, capitalization and whitespace variants are rejected rather than normalized.

| Integration from | Permitted command targets |
| --- | --- |
| planned | working |
| working | planned, implemented, verifying, ready |
| implemented | planned, working, verifying, ready |
| verifying | planned, working, implemented, ready |
| ready | working, verifying |
| released | none; create a new hotfix Integration |

Every route into `ready` evaluates dependencies, blockers, findings and configured verification gates. Returning to earlier work does not erase recorded checks or handoffs. Editing ready work resets it to working. Adding or reopening work reopens a completed Feature to active.

Features start planned or active. Planned can become active, blocked or archived; active can become blocked, completed or archived; blocked can become active, planned or archived; completed can reopen to active or be archived; archived can reopen to active or planned. Completion requires at least one Integration, all ready/released, with current readiness checks satisfied for ready work.

Persistence and backup restore validate current records and typed snapshots. Historical event payloads and opaque external provider responses retain their original vocabulary as provenance; they cannot extend the Control Plane's status catalog. `deployment_state` may be empty before the first worker step. Other required lifecycle fields cannot be empty.
