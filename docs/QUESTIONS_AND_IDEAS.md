# Questions, ideas, and next work

RCP stores owner questions as `Memory` records. They appear in search and feature resume; `list_questions` is the owner inbox, `get_question` returns the full record, and attention lists open questions addressed to the caller. A task question goes to its Integration assignee; otherwise it goes to the asking agent's human owner or the Product owner. Context IDs must refer to records in the same Product. Open blocking questions keep work out of the queue. If the asker holds the live Integration lease, the question also blocks that execution. Answering or withdrawing it resolves the blocker; the reconciler requests continuation at the existing wake target. A successful answer records a separate, durable decision quoting the question and answer.

## Owner chat workflow

1. Fetch `list_questions {"addressed_to":"me"}` and follow `next_cursor`. Present questions together with their options, recommendations, and context. Do not substitute questions from conversation memory.
2. Read the full question and its referenced records. Compare the owner's proposed answer with each question's recorded facts and constraints. If the answer contradicts the record or is unclear, explain the discrepancy and ask the owner to clarify.
3. Submit reviewed answers in one `answer_questions` batch. Use `override: true` only if the owner explicitly insists after the objection. One invalid or unauthorized answer rejects the whole batch. The addressee or their agent can answer. The original asker, the addressee, or their agents can use `withdraw_question` with a reason.

```json
{"action":"ask_question","feature_id":"feature-1","integration_id":"integration-1","data":{"title":"Which clients are in scope?","body":"The recorded constraint excludes preview clients.","kind":"decision","options":[{"id":"stable","label":"Stable clients only","consequence":"Preview clients remain out of scope."}],"recommendation":"stable","context_refs":["feature-1"],"blocking":true}}
```

```json
{"action":"answer_questions","data":{"answers":[{"question_id":"question-1","text":"Stable clients only.","option_id":"stable","validation":{"verdict":"consistent","note":"Matches the recorded scope constraint."}}]}}
```

## Ideas and dependencies

- Use `create_feature` with `status: "idea"`. The default remains `active`. A human Product member can promote an idea with `update_feature`. Agents ask by default; `request_idea_promotion` opens a decision for the Feature's human owner or Product owner. Choosing `approve` promotes the idea in the same transaction; `reject` leaves it unchanged. The Product owner can use `set_idea_promotion` to choose `agent_allowed` instead. `update_feature` can also accept an applicable answered `approval_question_id`.
- `propose_dependency` records `requires` (the source waits for the target to be completed) or `relates` (non-blocking). Humans can propose and accept at once with `accept: true`, or use `decide_dependency` later. An agent needs Product policy or an approving decision tied to the proposal (`request_dependency_decision`, then `decide_dependency` with the answer's question ID). Accepted `requires` edges cannot create a cycle. Cross-Product edges require write access to both endpoints and are omitted wherever either endpoint is hidden.
- `get_dependency_graph {"product_id":"product-1","include_proposed":true}` returns visible edges and each node's `unblocked` and `waiting_on` state. Proposals don't block execution.
- For a cross-Product approval, inspect the proposal in the dependency graph. The inbox question does not copy the other Product's details into the source Product.

## Queue and wake delivery

`next_work` uses the caller's human identity (the agent's owner for agent tokens). It returns eligible Integrations and `plan_feature` for ready Features that need Integrations. Lower Feature priority sorts first (default 100), then Feature creation time and Integration position. Skips explain unsatisfied prerequisites and blocking questions. Claim an unassigned Integration before starting; it becomes yours on claim. For an existing `lost` execution, use the execution recovery protocol rather than a fresh claim. `status_overview` gives visible per-Product counts and short lists, including recent completions and the caller's executions.

```json
{"action":"register_user_orchestrator","product_id":"product-1","data":{"harness":{"kind":"codex","tmux_target":"orch:0.0","session_ref":"session-id"}}}
```

Omit `product_id` for a cross-Product registration; use `unregister_user_orchestrator` with the same scope to stop it. RCP raises one `work_available` event when that queue becomes non-empty or when an answer needs a fallback wake. The supervisor uses only the registered target and a fixed prompt, not question contents. On wake, read `next_work` and the question inbox again. See [supervisor setup](SUPERVISOR.md) and the generated [command and route reference](AGENT_TOOLS.md).
