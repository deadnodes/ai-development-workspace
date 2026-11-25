package transport

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"releasecontrol/internal/agentguide"
	"releasecontrol/internal/application"
)

func registerAgentGuideRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent-kit", func(w http.ResponseWriter, r *http.Request) { write(w, 200, agentguide.Get()) })
}
func registerAgentGuideTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{Name: "get_agent_kit", Description: "Read workspace instructions and the rcp-handoff, rcp-delivery, and rcp-execution skills with checksums. Use release-control doctor to verify setup and get_tool_guide for tool contracts. For one-command workspace setup use release-control connect. Guidance is not permission for external mutations."}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return nil, agentguide.Get(), nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_agent_skill", Description: "Fetch an exact fixed rcp-handoff or rcp-delivery skill and content hash. Guidance preserves current policy and grants no execution authority."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Name string `json:"name"`
	}) (*mcp.CallToolResult, any, error) {
		skill, ok := agentguide.Lookup(in.Name)
		if !ok {
			return nil, nil, application.ErrNotFound
		}
		return nil, skill, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_tool_guide", Description: "Read the generated agent tool reference index or one exact tool, execute action, or HTTP route guide. Documentation does not grant authority."}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Name string `json:"name,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		return nil, agentguide.ToolGuide(in.Name), nil
	})
	for _, resource := range []struct{ name, uri string }{{agentguide.SkillName, agentguide.SkillURI}, {agentguide.DeliverySkillName, agentguide.DeliverySkillURI}, {agentguide.ExecutionSkillName, agentguide.ExecutionSkillURI}} {
		skill, _ := agentguide.Lookup(resource.name)
		meta := mcp.Meta{"sha256": skill.SHA256}
		server.AddResource(&mcp.Resource{URI: resource.uri, Name: resource.name, Description: "Canonical workflow skill; guidance grants no external authority", MIMEType: "text/markdown", Size: int64(len(skill.Content)), Meta: meta}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: resource.uri, MIMEType: "text/markdown", Text: skill.Content, Meta: meta}}}, nil
		})
	}
}
