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
	mcp.AddTool(server, &mcp.Tool{Name: "get_agent_kit", Description: "Read the workspace agent instructions and installable handoff skill. For one-command Codex workspace setup use release-control connect. Instructions are guidance, not permission for external mutations."}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return nil, agentguide.Get(), nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_agent_skill", Description: "Fetch rcp-handoff SKILL.md and content hash. Invoke before pausing, transferring context or finishing tracked feature work; save structured handoff through execute and verify through resume."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Name string `json:"name"`
	}) (*mcp.CallToolResult, any, error) {
		if in.Name != agentguide.SkillName {
			return nil, nil, application.ErrNotFound
		}
		return nil, agentguide.Get().Skill, nil
	})
	server.AddResource(&mcp.Resource{URI: agentguide.SkillURI, Name: agentguide.SkillName, Description: "Workspace handoff skill: structured canonical handoff and resume", MIMEType: "text/markdown"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: agentguide.SkillURI, MIMEType: "text/markdown", Text: agentguide.Get().Skill.Content}}}, nil
	})
}
