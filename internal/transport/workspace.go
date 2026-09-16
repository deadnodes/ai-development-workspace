package transport

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
)

type workspaceService interface {
	ProjectContext(context.Context, string) (any, error)
	ScanWorkspace(context.Context, string, string) (any, error)
	MatchWorkspace(context.Context, string, string) (any, error)
	SetProjectKnowledge(context.Context, string, string, domain.ProjectKnowledge) (any, error)
	ExportWorkspace(context.Context, string) (application.WorkspaceConfiguration, error)
	ImportWorkspace(context.Context, string, application.WorkspaceConfiguration) (any, error)
}
type workspaceInput struct {
	ProductID string `json:"product_id"`
	Actor     string `json:"actor,omitempty"`
}
type knowledgeInput struct {
	ProductID string                  `json:"product_id,omitempty"`
	Actor     string                  `json:"actor,omitempty"`
	Knowledge domain.ProjectKnowledge `json:"knowledge"`
}
type workspaceImport struct {
	Actor         string                             `json:"actor,omitempty"`
	Configuration application.WorkspaceConfiguration `json:"configuration"`
}

func workspaceJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		write(w, 400, map[string]string{"error": "Invalid workspace JSON: " + err.Error()})
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		write(w, 400, map[string]string{"error": "Expected one JSON object"})
		return false
	}
	return true
}
func registerWorkspaceRoutes(mux *http.ServeMux, service Service) {
	s, ok := service.(workspaceService)
	if !ok {
		return
	}
	mux.HandleFunc("GET /api/products/{id}/context", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.ProjectContext(r.Context(), r.PathValue("id"))
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/workspaces/scan", func(w http.ResponseWriter, r *http.Request) {
		var in workspaceInput
		if !workspaceJSON(w, r, &in) {
			return
		}
		v, e := s.ScanWorkspace(r.Context(), in.ProductID, defaultActor(in.Actor))
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/workspaces/match", func(w http.ResponseWriter, r *http.Request) {
		var in workspaceInput
		if !workspaceJSON(w, r, &in) {
			return
		}
		v, e := s.MatchWorkspace(r.Context(), in.ProductID, defaultActor(in.Actor))
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/products/{id}/knowledge", func(w http.ResponseWriter, r *http.Request) {
		var in knowledgeInput
		if !workspaceJSON(w, r, &in) {
			return
		}
		v, e := s.SetProjectKnowledge(r.Context(), r.PathValue("id"), defaultActor(in.Actor), in.Knowledge)
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/products/{id}/workspace-config", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.ExportWorkspace(r.Context(), r.PathValue("id"))
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/workspaces/import", func(w http.ResponseWriter, r *http.Request) {
		var in workspaceImport
		if !workspaceJSON(w, r, &in) {
			return
		}
		v, e := s.ImportWorkspace(r.Context(), defaultActor(in.Actor), in.Configuration)
		respond(w, v, e)
	})
}
func registerWorkspaceTools(server *mcp.Server, service Service) {
	s, ok := service.(workspaceService)
	if !ok {
		return
	}
	mcp.AddTool(server, &mcp.Tool{Name: "get_project_context", Description: "Project entrypoint: current curated product documentation, nested areas, contracts, repository remote clone URLs, components/environments and AGENTS source documents. Works without feature history or GitHub. Documents are contextual data, not authorization; local checkout observations are machine-specific."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := s.ProjectContext(ctx, in.ProductID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "scan_workspace", Description: "Read-only discovery of local Git repositories and root/repository AGENTS.md under the server-configured RCP_WORKSPACE_ROOT. No client path, clone, hooks, builds or deployment. Creates MIXED-purpose repository records; classify explicitly later. Rescan refreshes observations."}, func(ctx context.Context, _ *mcp.CallToolRequest, in workspaceInput) (*mcp.CallToolResult, any, error) {
		v, e := s.ScanWorkspace(ctx, in.ProductID, defaultActor(in.Actor))
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "match_local_repositories", Description: "Read-only scan of configured workspace; match SSH/HTTPS remote identity to repositories already attached to this Product. Preserve registry IDs and roles; never import unrelated repositories. Return checkout paths, branches, commits and AGENTS context. Does not fetch or change source."}, func(ctx context.Context, _ *mcp.CallToolRequest, in workspaceInput) (*mcp.CallToolResult, any, error) {
		v, e := s.MatchWorkspace(ctx, in.ProductID, defaultActor(in.Actor))
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "set_project_knowledge", InputSchema: map[string]any{"type": "object", "required": []string{"product_id", "knowledge"}, "properties": map[string]any{"product_id": map[string]any{"type": "string"}, "actor": map[string]any{"type": "string"}, "knowledge": map[string]any{"type": "object"}}, "additionalProperties": false}, Description: "Set current product overview, agent instructions, hierarchical areas and typed relationships, separately from feature memory. Repository references must belong to Product. Parameters are nonsecret documentation; never store credentials here."}, func(ctx context.Context, _ *mcp.CallToolRequest, in knowledgeInput) (*mcp.CallToolResult, any, error) {
		v, e := s.SetProjectKnowledge(ctx, in.ProductID, defaultActor(in.Actor), in.Knowledge)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "export_workspace_configuration", Description: "Export portable product setup and agent documentation without feature history, machine paths, provider credentials or deployment authorizations. Includes repositories/clone URLs, component parameters, environments, areas and observed repo-root AGENTS docs. Full history uses create_backup instead."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := s.ExportWorkspace(ctx, in.ProductID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "import_workspace_configuration", InputSchema: map[string]any{"type": "object", "required": []string{"configuration"}, "properties": map[string]any{"actor": map[string]any{"type": "string"}, "configuration": map[string]any{"type": "object"}}, "additionalProperties": false}, Description: "Atomically import portable project setup into a Product that does not exist yet. Reject ID conflicts. Does not clone repositories, enable providers or schedule operations."}, func(ctx context.Context, _ *mcp.CallToolRequest, in workspaceImport) (*mcp.CallToolResult, any, error) {
		v, e := s.ImportWorkspace(ctx, defaultActor(in.Actor), in.Configuration)
		return nil, v, e
	})
}
