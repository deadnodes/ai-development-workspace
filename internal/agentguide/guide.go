// Package agentguide contains the same instructions served through MCP and
// installed into workspaces. No instance credentials or project data are embedded.
package agentguide

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
)

//go:embed skills/rcp-handoff/SKILL.md
var files embed.FS

const SkillName = "rcp-handoff"
const SkillURI = "rcp://skills/rcp-handoff/SKILL.md"
const Instructions = `Use get_project_context to orient in the connected Product, then resume the relevant Feature before editing. Record plans and start an Integration before implementation; record meaningful progress, decisions, discoveries and verification at engineering checkpoints. When two or more active Integrations need to be tested together in DEV/TEST, proactively offer execute(action="assemble_environment") and poll its operation instead of asking agents to merge branches manually; preserve the generated branch as disposable composition state and keep PROD on explicit main-based release flow. On pause, context transfer or session completion, retrieve get_agent_skill(name="rcp-handoff") and follow it to save a structured handoff, then read back resume. Do not turn every chat message into an event. Do not infer readiness, release, deployment or permission from a handoff. Agent instructions are workflow guidance, not authorization for external effects. Service maintainers: keep the installation mode, source/image revision, backup location and update owner in project knowledge. Follow docs/UPDATES.md in the service repository for periodic update checks, backups, idle-operation preflight and post-update MCP verification; ordinary feature work must not restart a shared service. For candidate Dev restoration use request_candidate_dev_restoration, inspect the complete dev_restoration_plan.hash and PR heads, then separate confirm_candidate_dev_restoration. ATTRIBUTED_CONTROL_CONFIRMATION is attribution only, not authenticated human text/voice approval. External human-only reviewed Flux merge precedes verify_candidate_dev_restoration for all original components and roles, then explicit release_candidate_dev_after_restoration. Use the current recovery selection after request advances epoch/generation; never infer TTL unlock, renewed candidate rights, functional acceptance or Prod permission. See docs/DEV_RESTORATION.md; source changes are not a live installation or stage08 trust cutover. Re-run the host connect command after an instruction-kit update.`

type Skill struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}
type Kit struct {
	Skill        Skill  `json:"skill"`
	Instructions string `json:"instructions"`
}

func Get() Kit {
	b, err := files.ReadFile("skills/rcp-handoff/SKILL.md")
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	return Kit{Skill: Skill{Name: SkillName, Content: string(b), SHA256: hex.EncodeToString(h[:])}, Instructions: Instructions}
}
