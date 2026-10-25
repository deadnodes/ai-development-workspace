package transport_test

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"releasecontrol/internal/agentconnect"
	"releasecontrol/internal/agentguide"
	"releasecontrol/internal/application"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
	"strings"
	"testing"
)

func TestAgentKitHTTPMCPAndResourceAgree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	server := httptest.NewServer(transport.New(application.New(store), transport.Options{}))
	defer server.Close()
	response, e := http.Get(server.URL + "/api/agent-kit")
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	var kit agentguide.Kit
	if e = json.NewDecoder(response.Body).Decode(&kit); e != nil {
		t.Fatal(e)
	}
	if kit != agentguide.Get() {
		t.Fatal("HTTP skill differs")
	}
	session, e := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	if session.InitializeResult().Instructions != agentguide.Instructions {
		t.Fatal("MCP bootstrap instructions absent")
	}
	for _, name := range []string{"get_agent_kit", "get_agent_skill"} {
		args := map[string]any{}
		if name == "get_agent_skill" {
			args["name"] = "rcp-handoff"
		}
		r, e := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil || r.IsError {
			t.Fatalf("%s: %v %+v", name, e, r)
		}
		b, _ := json.Marshal(r.StructuredContent)
		var raw map[string]any
		if e = json.Unmarshal(b, &raw); e != nil {
			t.Fatal(e)
		}
		if name == "get_agent_skill" && raw["sha256"] != kit.Skill.SHA256 {
			t.Fatal("MCP skill hash mismatch")
		}
	}
	resource, e := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: agentguide.SkillURI})
	if e != nil {
		t.Fatal(e)
	}
	if len(resource.Contents) != 1 || resource.Contents[0].Text != kit.Skill.Content {
		t.Fatal("MCP resource differs")
	}
	for _, skill := range []agentguide.Skill{kit.Skill, kit.DeliverySkill} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_agent_skill", Arguments: map[string]any{"name": skill.Name}})
		if err != nil || result.IsError {
			t.Fatalf("skill fetch failed %s: %v", skill.Name, err)
		}
		raw, _ := json.Marshal(result.StructuredContent)
		var got agentguide.Skill
		if json.Unmarshal(raw, &got) != nil || got != skill {
			t.Fatal("HTTP/MCP skill mismatch")
		}
		uri := "rcp://skills/" + skill.Name + "/SKILL.md"
		resource, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil || len(resource.Contents) != 1 || resource.Contents[0].Text != skill.Content || resource.Contents[0].Meta["sha256"] != skill.SHA256 {
			t.Fatal("skill resource mismatch", err)
		}
	}
	resources, err := session.ListResources(ctx, &mcp.ListResourcesParams{})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range resources.Resources {
		if r.URI == agentguide.SkillURI || r.URI == agentguide.DeliverySkillURI {
			skill, _ := agentguide.Lookup(r.Name)
			if r.Meta["sha256"] != skill.SHA256 || r.Size != int64(len(skill.Content)) {
				t.Fatal("resource checksum/size differs")
			}
			if found[r.URI] {
				t.Fatal("duplicate skill resource")
			}
			found[r.URI] = true
		}
	}
	if len(found) != 2 {
		t.Fatal("fixed resources missing")
	}
	for _, name := range []string{"", "rcp-delivery/SKILL.md", "RCP-DELIVERY", "other"} {
		bad, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_agent_skill", Arguments: map[string]any{"name": name}})
		if err == nil && !bad.IsError {
			t.Fatal("unknown skill accepted")
		}
	}
	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "rcp://skills/../private"}); err == nil {
		t.Fatal("unknown resource accepted")
	}
	for _, command := range []map[string]any{
		{"action": "create_product", "id": "p", "data": map[string]any{"name": "Product"}},
		{"action": "create_feature", "id": "f", "product_id": "p", "data": map[string]any{"title": "Feature", "problem": "Context is lost", "goal": "Handoff"}},
		{"action": "create_integration", "id": "i", "feature_id": "f", "data": map[string]any{"title": "Work", "objective": "Continue"}},
		{"action": "handoff", "feature_id": "f", "integration_id": "i", "data": map[string]any{"completed": []string{"Implemented API"}, "current": "Tests pending", "remaining": []string{"Acceptance check"}, "next": []string{"Run acceptance"}, "body": "Suggested skills: rcp-handoff; References: docs/CONTRACT.md"}},
	} {
		command["actor"] = "agent/skill-test"
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: command})
		if err != nil || result.IsError {
			data, _ := json.Marshal(result)
			t.Fatalf("handoff workflow %v failed: %v %s", command["action"], err, data)
		}
	}
	resumed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "resume", Arguments: map[string]any{"feature_id": "f"}})
	if err != nil || resumed.IsError {
		t.Fatalf("resume: %v", err)
	}
	saved, _ := json.Marshal(resumed.StructuredContent)
	if !strings.Contains(string(saved), "Run acceptance") {
		t.Fatal("handoff not in resumed context")
	}
	workspace := t.TempDir()
	binding, err := agentconnect.Install(ctx, agentconnect.Options{URL: server.URL, Workspace: workspace})
	if err != nil || binding.ProductID != "p" {
		t.Fatal("real service bounded discovery/install failed", binding, err)
	}
	for _, directory := range []string{".agents", ".claude"} {
		for _, skill := range []agentguide.Skill{kit.Skill, kit.DeliverySkill} {
			data, err := os.ReadFile(filepath.Join(workspace, directory, "skills", skill.Name, "SKILL.md"))
			if err != nil || !strings.HasPrefix(string(data), skill.Content) {
				t.Fatal("real service canonical install differs", skill.Name, err)
			}
		}
	}
	bad, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_agent_skill", Arguments: map[string]any{"name": "../../private"}})
	if e == nil && !bad.IsError {
		t.Fatal("unknown skill accepted")
	}
}
