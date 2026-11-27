package transport

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/agentguide"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
)

type agentGuideService interface {
	AgentKit(context.Context, string) (agentguide.Kit, error)
	AgentSkill(context.Context, string, string) (any, error)
}

func registerAgentGuideRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET /api/agent-kit", func(w http.ResponseWriter, r *http.Request) { write(w, 200, agentguide.Get()) })
}
func registerAgentGuideTools(server *mcp.Server, service Service, instructions string) {
	guide, _ := service.(agentGuideService)
	mcp.AddTool(server, &mcp.Tool{Name: "get_agent_kit", Description: "List built-in and optional Product skills with descriptions, when to load them, role assignments and checksums. Guidance grants no external authority."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		kit := agentguide.Get()
		if in.ProductID != "" {
			if guide == nil {
				return nil, nil, application.ErrNotFound
			}
			var err error
			kit, err = guide.AgentKit(ctx, in.ProductID)
			if err != nil {
				return nil, nil, err
			}
		}
		kit.Instructions = instructions
		return nil, kit, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_agent_skill", Description: "Fetch a built-in or Product skill by exact name, including Markdown, any supporting files and checksums."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Name      string `json:"name"`
		ProductID string `json:"product_id,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		if guide != nil {
			value, err := guide.AgentSkill(ctx, in.ProductID, in.Name)
			return nil, value, err
		}
		if in.ProductID != "" {
			return nil, nil, application.ErrNotFound
		}
		skill, ok := agentguide.Lookup(in.Name)
		if !ok {
			return nil, nil, application.ErrNotFound
		}
		return nil, skill, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "upsert_agent_skill", Description: "Publish Product-scoped Markdown and optional relative text files. Product owner or instance admin only."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
		domain.AgentSkillInput
	}) (*mcp.CallToolResult, any, error) {
		data := map[string]any{"name": in.Name, "description": in.Description, "body": in.Body}
		if in.Files != nil {
			data["files"] = in.Files
		}
		value, err := service.Execute(ctx, domain.Command{Action: "upsert_agent_skill", Actor: "agent", ProductID: in.ProductID, Data: data})
		return nil, value, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "delete_agent_skill", Description: "Delete an unassigned Product skill. Product owner or instance admin only."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
		Name      string `json:"name"`
	}) (*mcp.CallToolResult, any, error) {
		value, err := service.Execute(ctx, domain.Command{Action: "delete_agent_skill", Actor: "agent", ProductID: in.ProductID, Data: map[string]any{"name": in.Name}})
		return nil, value, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "set_product_agent_roles", Description: "Assign existing Product or built-in skills to the orchestrator and worker. Product owner only."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
		domain.AgentRoles
	}) (*mcp.CallToolResult, any, error) {
		value, err := service.Execute(ctx, domain.Command{Action: "set_product_agent_roles", Actor: "agent", ProductID: in.ProductID, Data: map[string]any{"orchestrator_skill": in.OrchestratorSkill, "worker_skill": in.WorkerSkill}})
		return nil, value, err
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
