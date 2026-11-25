---
name: rcp-execution
description: Run fenced, resumable multi-Integration work through Release Control Plane execution groups.
---

# RCP execution protocol

Work only on Integrations assigned to your human or unassigned. `NOT_ASSIGNEE`
means pick other work; never retry or take over that Integration.

1. At the start of multi-Feature work, create one execution group with `create_execution_group`. Set `orchestrator_harness` and `tmux_target` when available; add each Integration as a member with its own desired state.
2. Claim each Integration you work on with `claim_execution`. Keep its returned fence. Send `heartbeat_execution` while working and before the lease expires.
3. Include `execution_fence` in every `record_progress` and `transition_integration` write. A `STALE_FENCE` means stop writes and restore current state; never retry using an old fence.
4. After idling between turns your lease may expire (`lost`). Resume your own execution with any write that carries your current fence, normally `heartbeat_execution(fence)`; the fence stays the same. Use `claim_execution` only for `unassigned`, `exited_incomplete` or `paused` members. Use `takeover_execution` only for another executor's `lost`/`stalled` work, after a delivered event identifies it and you have checked current state.
5. When an Integration reaches its `desired_state`, release it with `release_execution(outcome="completed")`. If blocked or interrupted, report the truthful outcome and reference the blocker or operation. `ready` does not mean released.
6. On a `[RCP supervisor]` wake message, call `get_execution_group`, inspect every member's `next_action`, and follow it. Never restart a completed member.
7. At turn end, report the turn with `report_turn_end` when configured. The supervisor hook expects a genuine completed-turn notification; it does not grant permission or prove work finished.
8. After restarting in a new session, call `set_execution_group_harness` with the current harness/session and tmux target before continuing.

Execution fences protect RCP writes only. They do not stop direct Git writes, authorize source changes outside the user's task, or authorize builds, deployments, merges, or production operations.
