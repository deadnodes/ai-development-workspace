// Package agentguide contains the same instructions served through MCP and
// installed into workspaces. No instance credentials or project data are embedded.
package agentguide

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
)

//go:embed skills/rcp-handoff/SKILL.md skills/rcp-delivery/SKILL.md skills/provenance.json
var files embed.FS

const SkillName = "rcp-handoff"
const SkillURI = "rcp://skills/rcp-handoff/SKILL.md"
const DeliverySkillName = "rcp-delivery"
const DeliverySkillURI = "rcp://skills/rcp-delivery/SKILL.md"
const Instructions = `Read the nearest .release-control.json and use get_project_context(product_id) to orient in the connected Product, then resume the relevant Feature before editing. Navigate with bounded get_state Product/Feature metadata, not a whole-instance snapshot; resume contains history and is not a compact export. Record plans and start an Integration before implementation; record meaningful progress, decisions, discoveries and verification at engineering checkpoints. Retrieve get_agent_skill(name="rcp-delivery") for Feature delivery, build/worker history and composition conflicts. Retrieve get_agent_skill(name="rcp-handoff") for checkpoints, pause, context transfer or session completion. Before selecting delivery, check repository adoption, current branch/release policy and the human-authorized target/effect. Existing main-first and human-only reviewed Flux merge rules, including Dev, remain authoritative until an explicit scoped cutover. Do not infer adoption, enable automatic assembly or select all active work merely because assemble_environment is exposed or multiple Integrations exist. Use live schemas and the exact current selection/CAS for adopted RCP-managed paths; update_environment_feature patches only the selected Feature. Follow the returned operation: uncertainty or observation timeout requires inspecting that same handle, not a replacement build. Use scoped bounded worker history where supported, without full-State or unbounded worker-job fallback. When get_operation returns agent_action.type=RESOLVE_CONFLICT, the requesting agent resolves externally against exact frozen branches/SHAs and both Features' acceptance through claim_composition_conflict/record_conflict_resolution/verify_conflict_resolution. RCP does not spawn another agent. UNAVAILABLE requires context reconciliation; never guess partial inputs. Reconcile the original composition explicitly with resolution_conflict_ids only within existing build/GitOps authorization. Save record_progress and structured handoff in the real Product/Feature/Integration, then read back resume and verify both record IDs for current work before publication or successful completion. Do not turn every chat message into an event or infer readiness, release, deployment or permission from a handoff. Agent instructions are workflow guidance, not authorization for external effects. Service maintainers: keep installation mode, source/image revision, backup location and update owner in project knowledge. Follow docs/UPDATES.md for update checks, backups, idle-operation preflight and post-update MCP verification; ordinary feature work must not restart a shared service. For candidate Dev restoration use request_candidate_dev_restoration, inspect the complete dev_restoration_plan.hash and PR heads, then separate confirm_candidate_dev_restoration. ATTRIBUTED_CONTROL_CONFIRMATION is attribution only, not authenticated human text/voice approval. External human-only reviewed Flux merge precedes verify_candidate_dev_restoration for all original components and roles, then explicit release_candidate_dev_after_restoration. Use the current recovery selection after request advances epoch/generation; never infer TTL unlock, renewed candidate rights, functional acceptance or Prod permission. See docs/DEV_RESTORATION.md; source changes are not a live installation or stage08 trust cutover. Re-run the host connect command after an instruction-kit update.`

type Skill struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}
type Kit struct {
	Skill         Skill  `json:"skill"`
	DeliverySkill Skill  `json:"delivery_skill"`
	Instructions  string `json:"instructions"`
}

func Get() Kit {
	return Kit{Skill: mustSkill(SkillName), DeliverySkill: mustSkill(DeliverySkillName), Instructions: Instructions}
}

// Lookup serves only the two fixed identities; untrusted names never become paths.
func Lookup(name string) (Skill, bool) {
	switch name {
	case SkillName, DeliverySkillName:
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
