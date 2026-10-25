package agentguide

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalSkillProvenance(t *testing.T) {
	raw, err := files.ReadFile("skills/provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	var sources []struct{ Name, Repository, Commit, Path, SHA256 string }
	if json.Unmarshal(raw, &sources) != nil || len(sources) != 2 {
		t.Fatal("invalid skill provenance")
	}
	expected := map[string]string{SkillName: "ae1d437e8b2e49c6b5ba83f85867ee5e70856abb35c65bf1c82a6ea38d5d2179", DeliverySkillName: "e21f283dcdd217f6b1e3ca37cebc26f5931723634f8287cbb70b06cc4e5c1feb"}
	for _, source := range sources {
		sha, ok := expected[source.Name]
		if !ok {
			t.Fatal("unknown/duplicate skill provenance")
		}
		delete(expected, source.Name)
		if source.Repository != "https://github.com/deadnodes/ai-tools" || source.Commit != "cd8bd058ce49fb96e96b6777725b03f9c8255bd5" || source.Path != ".workspace/skills/"+source.Name+"/SKILL.md" || source.SHA256 != sha {
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
