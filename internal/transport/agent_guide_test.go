package transport_test

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"releasecontrol/internal/agentguide"
	"releasecontrol/internal/application"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
	"strings"
	"testing"
)

func TestAgentKitHTTPMCPAndResourceAgree(t *testing.T) {
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
	bad, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_agent_skill", Arguments: map[string]any{"name": "../../private"}})
	if e == nil && !bad.IsError {
		t.Fatal("unknown skill accepted")
	}
}
