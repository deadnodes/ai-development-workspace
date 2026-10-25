package agentguide

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestInstructionsSeparateGeneralDevPolicyFromRestoration(t *testing.T) {
	general, restoration, ok := strings.Cut(Instructions, "For candidate Dev restoration ")
	if !ok {
		t.Fatal("missing restoration protocol")
	}
	for _, guard := range []string{"conditional Dev Flux exception", "actual owner instruction", "complete allowed Dev scope", "independent tests and fresh critic review of the current full head", "explicit squash with a server-side SHA lock", "retrieve rcp-delivery for the complete conditions", "Production, mixed, shared and unknown-scope Flux merges remain human-only", "no additional configuration, CI security, native-path adoption, authentication or permission rights"} {
		if !strings.Contains(general, guard) {
			t.Fatalf("missing current Dev policy guard %q", guard)
		}
	}
	if strings.Contains(general, "human-only reviewed Flux") || strings.Contains(general, "including Dev") || strings.Contains(general, "until an explicit scoped cutover") {
		t.Fatal("obsolete blanket Dev merge restriction")
	}
	const originalRestoration = "use request_candidate_dev_restoration, inspect the complete dev_restoration_plan.hash and PR heads, then separate confirm_candidate_dev_restoration. ATTRIBUTED_CONTROL_CONFIRMATION is attribution only, not authenticated human text/voice approval. External human-only reviewed Flux merge precedes verify_candidate_dev_restoration for all original components and roles, then explicit release_candidate_dev_after_restoration. Use the current recovery selection after request advances epoch/generation; never infer TTL unlock, renewed candidate rights, functional acceptance or Prod permission. See docs/DEV_RESTORATION.md; source changes are not a live installation or stage08 trust cutover. Re-run the host connect command after an instruction-kit update."
	if restoration != originalRestoration {
		t.Fatal("source-specific Dev restoration protocol changed")
	}
}

func TestCanonicalSkillProvenance(t *testing.T) {
	raw, err := files.ReadFile("skills/provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	var sources []struct{ Name, Repository, Commit, Path, SHA256 string }
	if json.Unmarshal(raw, &sources) != nil || len(sources) != 2 {
		t.Fatal("invalid skill provenance")
	}
	expected := map[string]string{SkillName: "ae1d437e8b2e49c6b5ba83f85867ee5e70856abb35c65bf1c82a6ea38d5d2179", DeliverySkillName: "6ea608ca7c3d1503537a65610557ca07b6e36b569a9a2445a904ffcba062961e"}
	for _, source := range sources {
		sha, ok := expected[source.Name]
		if !ok {
			t.Fatal("unknown/duplicate skill provenance")
		}
		delete(expected, source.Name)
		if source.Repository != "https://github.com/deadnodes/ai-tools" || source.Commit != "4a25d7edcdda49aa550d73967f156a9e572cd284" || source.Path != ".workspace/skills/"+source.Name+"/SKILL.md" || source.SHA256 != sha {
			t.Fatal("canonical source changed without deliberate snapshot update")
		}
		skill, ok := Lookup(source.Name)
		if !ok {
			t.Fatal("missing canonical skill")
		}
		h := sha256.Sum256([]byte(skill.Content))
		if hex.EncodeToString(h[:]) != sha || skill.SHA256 != sha {
			t.Fatal("canonical bytes differ")
		}
	}
	for _, name := range []string{"", "../rcp-delivery", "RCP-HANDOFF", "rcp-handoff/SKILL.md", "other"} {
		if _, ok := Lookup(name); ok {
			t.Fatal("unknown skill accepted")
		}
	}
	for _, guard := range []string{"human-only reviewed Flux", "ATTRIBUTED_CONTROL_CONFIRMATION", "request_candidate_dev_restoration", "confirm_candidate_dev_restoration", "all original components and roles", "epoch/generation", "TTL unlock", "update owner", "rcp-delivery", "bounded get_state"} {
		if !strings.Contains(Instructions, guard) {
			t.Fatalf("missing instruction guard %q", guard)
		}
	}
	if strings.Contains(Instructions, "get_product_state") || strings.Contains(Instructions, "proactively offer") {
		t.Fatal("unbounded or implicit adoption guidance")
	}
}
