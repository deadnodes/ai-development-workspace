# Questions, ideas, and next work

RCP stores owner questions as `Memory` records. They appear in search and feature resume; `list_questions` is the owner inbox, `get_question` returns the full record, and attention lists open questions addressed to the caller. A task question goes to its Integration assignee; otherwise it goes to the asking agent's human owner or the Product owner. Context IDs must refer to records in the same Product. Only the Integration assignee can ask a blocking Integration question, and only the live lease holder can do so during active execution. A Feature-wide blocking question requires the Product owner. Other members can ask nonblocking questions. A successful answer records a separate, durable decision quoting the question and answer.

## Owner chat workflow

1. Fetch `list_questions {"addressed_to":"me"}` and follow `next_cursor`. Present questions together with their options, recommendations, and context. Do not substitute questions from conversation memory.
2. Read the full question and its referenced records. Compare the owner's proposed answer with each question's recorded facts and constraints. If the answer contradicts the record or is unclear, explain the discrepancy and ask the owner to clarify.
3. Submit reviewed answers in one `answer_questions` batch. Use `override: true` only if the owner explicitly insists after the objection. One invalid or unauthorized answer rejects the whole batch. The addressee or their agent can answer ordinary questions. Only the human Product owner can answer a server-generated promotion or dependency decision. The original asker, the addressee, or their agents can use `withdraw_question` with a reason.

```json
{"action":"ask_question","feature_id":"feature-1","integration_id":"integration-1","data":{"title":"Which clients are in scope?","body":"The recorded constraint excludes preview clients.","kind":"decision","options":[{"id":"stable","label":"Stable clients only","consequence":"Preview clients remain out of scope."}],"recommendation":"stable","context_refs":["feature-1"],"blocking":true}}
```

```json
{"action":"answer_questions","data":{"answers":[{"question_id":"question-1","text":"Stable clients only.","option_id":"stable","validation":{"verdict":"consistent","note":"Matches the recorded scope constraint."}}]}}
```

## Ideas and dependencies

- Use `create_feature` with `status: "idea"`. The default remains `active`. The Product owner can promote an idea with `update_feature`. Agents ask by default; `request_idea_promotion` opens a decision for the Product owner, regardless of the Feature's editable owner. Approval promotes the exact requested revision in the same transaction. Edits supersede the stale request and remove it from the actionable inbox without deleting it. The Product owner can use `set_idea_promotion` to choose `agent_allowed` instead.
- Moving a Feature to `blocked`, `idea`, or `archived` requires the Product owner (or their agent) or the assignee of every assigned Integration that is not `ready` or `released`. Otherwise the transition returns `NOT_ASSIGNEE`.
- Restore an archived Feature to its pre-archive status. For legacy records, event history includes implicit reopens caused by Integration changes. If the status cannot be recovered or no longer satisfies completion requirements, the Product owner restores it to `planned`; the service records an audit warning.
- `propose_dependency` records `requires` (the source waits for the target to be completed) or `relates` (non-blocking). The source Product owner can propose and accept at once with `accept: true`, or use `decide_dependency` later. An agent needs Product policy or an approval for the exact edge, action, and current request. Use `request_dependency_decision` with `decision: "accept"` (default) or `"reject"`, then pass the applicable answer's question ID to `decide_dependency`. Accepted `requires` edges cannot create a cycle. Cross-Product edges require write access to both endpoints and are omitted wherever either endpoint is hidden.
- `get_dependency_graph {"product_id":"product-1","include_proposed":true}` returns visible edges and each node's `unblocked` and `waiting_on` state. A hidden, unfinished prerequisite still makes `unblocked` false, but its identifier and edge details are omitted. Proposals don't block execution.
- A direct accept or reject withdraws the linked open approval question from the inbox and preserves it in history.
- For a cross-Product approval, inspect the proposal in the dependency graph. The inbox question does not copy the other Product's details into the source Product.

## Queue and wake delivery

`next_work` uses the caller's human identity (the agent's owner for agent tokens). It returns eligible Integrations and `plan_feature` for ready Features that need Integrations. Lower Feature priority sorts first (default 100), then Feature creation time and Integration position. Skips explain unsatisfied prerequisites and blocking questions. Claim an unassigned Integration before starting; it becomes yours on claim. For an existing `lost` execution, use the execution recovery protocol rather than a fresh claim. `status_overview` gives visible per-Product counts and short lists, including recent completions and the caller's executions.

Each human runs their own supervisor with their own token on a host and tmux server they own. Wake targets are trusted only within that supervisor's own tmux server. Do not share a tmux server between humans.

```json
{"action":"register_user_orchestrator","product_id":"product-1","data":{"harness":{"kind":"codex","tmux_target":"orch:0.0","session_ref":"session-id"}}}
```

Omit `product_id` for a cross-Product registration; use `unregister_user_orchestrator` with the same scope to stop it. RCP raises a typed queue-availability wake when the queue becomes non-empty, retires it when the queue drains, and replaces it if its Product is no longer eligible while work remains elsewhere. Question continuations coalesce by registration and work scope; the event records the triggering `question_ids`. Once a blocker is resolved and all prerequisites permit starting, only the current assignee sees a `resume_integration` item with the fence and next action. This also covers release work when the Integration is already `ready`. Use `report_execution` with `state: "running"` and that fence. Reassignment invalidates old fences and wake targets and readdresses open Integration blockers to the new assignee. The supervisor uses fixed prompts and server-generated causes, not caller reasons or question contents. See [supervisor setup](SUPERVISOR.md) and the generated [command and route reference](AGENT_TOOLS.md).
