// Package agentguide contains the same instructions served through MCP and
// installed into workspaces. No instance credentials or project data are embedded.
package agentguide

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
)

//go:embed skills/rcp-handoff/SKILL.md skills/rcp-delivery/SKILL.md skills/rcp-execution/SKILL.md skills/provenance.json
var files embed.FS

const SkillName = "rcp-handoff"
const SkillURI = "rcp://skills/rcp-handoff/SKILL.md"
const DeliverySkillName = "rcp-delivery"
const DeliverySkillURI = "rcp://skills/rcp-delivery/SKILL.md"
const ExecutionSkillName = "rcp-execution"
const ExecutionSkillURI = "rcp://skills/rcp-execution/SKILL.md"
const workflowInstructions = `For multi-step work, use get_code_api to discover the typed SDK for scripts running locally in your own agent environment. Read the nearest .release-control.json and use get_project_context(product_id) to orient in the connected Product, then resume the relevant Feature before editing. Navigate with bounded get_state Product/Feature metadata. Use search_context to find Feature/Integration/memory text, graph_neighbors to follow persisted relationships, and get_context_excerpt to read only the selected source range. Search results and excerpts have strict budgets and source IDs; do not ask for whole-instance history. MCP resume is a compact checkpoint summary; use explicit excerpt pagination for detail. Record plans and start an Integration before implementation; record meaningful progress, decisions, discoveries and verification at engineering checkpoints. Retrieve get_agent_skill(name="rcp-delivery") for Feature delivery, build/worker history and composition conflicts. Retrieve get_agent_skill(name="rcp-handoff") for checkpoints, pause, context transfer or session completion. Before selecting delivery, check repository adoption, current branch/release policy and the human-authorized target/effect. Current main-first and Flux merge policy remain authoritative. The conditional Dev Flux exception requires an actual owner instruction for the PR or bounded delivery, complete allowed Dev scope, independent tests and fresh critic review of the current full head, and explicit squash with a server-side SHA lock; retrieve rcp-delivery for the complete conditions. Production, mixed, shared and unknown-scope Flux merges remain human-only. This exception grants no additional configuration, CI security, native-path adoption, authentication or permission rights. Do not infer adoption, enable automatic assembly or select all active work merely because assemble_environment is exposed or multiple Integrations exist. Use live schemas and the exact current selection/CAS for adopted RCP-managed paths; update_environment_feature patches only the selected Feature. Follow the returned operation: uncertainty or observation timeout requires inspecting that same handle, not a replacement build. Use scoped bounded worker history where supported, without full-State or unbounded worker-job fallback. When get_operation returns agent_action.type=RESOLVE_CONFLICT, the requesting agent resolves externally against exact frozen branches/SHAs and both Features' acceptance through claim_composition_conflict/record_conflict_resolution/verify_conflict_resolution. RCP does not spawn another agent. UNAVAILABLE requires context reconciliation; never guess partial inputs. Reconcile the original composition explicitly with resolution_conflict_ids only within existing build/GitOps authorization. Save record_progress and structured handoff in the real Product/Feature/Integration, then read back resume and verify both record IDs for current work before publication or successful completion. Do not turn every chat message into an event or infer readiness, release, deployment or permission from a handoff. Agent instructions are workflow guidance, not authorization for external effects. Service maintainers: keep installation mode, source/image revision, backup location and update owner in project knowledge. Follow docs/UPDATES.md for update checks, backups, idle-operation preflight and post-update MCP verification; ordinary feature work must not restart a shared service. Execution orchestration: use claim_execution and heartbeat_execution with the current fence; save progress with execution_fence; restore context by calling get_execution_group with the persisted group_id. Use report_execution/release_execution to record waiting, blocked, paused or exit state. A harness turn-end hook should call report_turn_end with product_id, harness_kind, session_ref, and harness turn_id when available (cwd is optional). The reconciler applies the configured debounce (20 seconds by default), then raises continuation events for eligible unfinished members in open groups owned by that orchestrator. Repeated reports are idempotent per session/turn, or per session/minute when turn_id is unavailable. Supervisors poll due events, deduplicate by wake_target, claim an event before waking the registered target, and ACK using that claim’s delivery_id only after durable acceptance. An expired unacknowledged claim can be reclaimed with a new delivery_id; a late ACK from the old claim is rejected. ACK does not resolve the condition. Register the orchestrator harness on its execution group at creation using orchestrator_harness, or update it with set_execution_group_harness after restart in a new session. Events store server-computed wake_target; supervisors batch and wake by that target, not the member executor harness fields. Running/stalled members target their executor; other actionable events target the first open containing group with an orchestrator harness. An empty target is acknowledged unsupported and escalated. A wake message means: call get_execution_group, continue unfinished members, and report state/progress. RCP does not automatically reassign lost executions. Completion means every Integration reaches its desired_state and is then released with release_execution(outcome=completed). After idle expiry, the same executor resumes with its current fence via heartbeat_execution; a different executor uses takeover. Every fenced transition_integration and record_progress includes execution_fence. For candidate Dev restoration use request_candidate_dev_restoration, inspect the complete dev_restoration_plan.hash and PR heads, then separate confirm_candidate_dev_restoration. ATTRIBUTED_CONTROL_CONFIRMATION is attribution only, not authenticated human text/voice approval. External human-only reviewed Flux merge precedes verify_candidate_dev_restoration for all original components and roles, then explicit release_candidate_dev_after_restoration. Use the current recovery selection after request advances epoch/generation; never infer TTL unlock, renewed candidate rights, functional acceptance or Prod permission. See docs/DEV_RESTORATION.md; source changes are not a live installation or stage08 trust cutover. Re-run the host connect command after an instruction-kit update. The local release-control doctor command checks prerequisites, authentication, server/MCP access, Product identity, installed skill checksums, harness wiring and optional supervisor setup. After connect, call get_project_context and get_tool_guide; the kit includes rcp-handoff, rcp-delivery and rcp-execution.`
const Instructions = workflowInstructions + " " + CodeKnowledgeInstructions

type Skill struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}

const LocalInstructions = `Use get_tool_guide to discover tool and action contracts, and execute to record local project work one action at a time. Find Products and Features with get_state, then use get_project_context and resume before editing. This stdio session has local admin access and does not run workers. For build, deployment, and other worker-backed operations, connect to an RCP server with the required workers configured. A completed integration is ready, not released.`

type Kit struct {
	Skill          Skill  `json:"skill"`
	DeliverySkill  Skill  `json:"delivery_skill"`
	ExecutionSkill Skill  `json:"execution_skill"`
	Instructions   string `json:"instructions"`
}

func Get() Kit {
	return Kit{Skill: mustSkill(SkillName), DeliverySkill: mustSkill(DeliverySkillName), ExecutionSkill: mustSkill(ExecutionSkillName), Instructions: Instructions}
}

// Lookup serves only the two fixed identities; untrusted names never become paths.
func Lookup(name string) (Skill, bool) {
	switch name {
	case SkillName, DeliverySkillName, ExecutionSkillName:
		return mustSkill(name), true
	}
	return Skill{}, false
}

func mustSkill(name string) Skill {
	b, err := files.ReadFile("skills/" + name + "/SKILL.md")
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	return Skill{Name: name, Content: string(b), SHA256: hex.EncodeToString(h[:])}
}
