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
	mcp.AddTool(server, &mcp.Tool{Name: "get_agent_kit", Description: "Read the workspace agent instructions and installable handoff and delivery skills. For one-command Codex workspace setup use release-control connect. Instructions are guidance, not permission for external mutations."}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
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
	for _, resource := range []struct{ name, uri string }{{agentguide.SkillName, agentguide.SkillURI}, {agentguide.DeliverySkillName, agentguide.DeliverySkillURI}} {
		skill, _ := agentguide.Lookup(resource.name)
		meta := mcp.Meta{"sha256": skill.SHA256}
		server.AddResource(&mcp.Resource{URI: resource.uri, Name: resource.name, Description: "Canonical workflow skill; guidance grants no external authority", MIMEType: "text/markdown", Size: int64(len(skill.Content)), Meta: meta}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: resource.uri, MIMEType: "text/markdown", Text: skill.Content, Meta: meta}}}, nil
		})
	}
}
