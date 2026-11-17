package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/domain"
)

type pipelineService interface {
	ListPipelines(context.Context, domain.ListPipelinesRequest) (domain.PipelinePage, error)
}

func registerPipelineRoutes(mux *http.ServeMux, service Service) {
	mux.HandleFunc("GET /api/pipelines", func(w http.ResponseWriter, r *http.Request) {
		s, ok := service.(pipelineService)
		if !ok {
			write(w, 501, map[string]string{"error": "pipeline service unavailable"})
			return
		}
		values, e := strconvQueryHistory(r)
		if e != nil {
			write(w, 400, map[string]string{"error": "invalid pipeline query"})
			return
		}
		raw, _ := json.Marshal(values)
		var in domain.ListPipelinesRequest
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil {
			write(w, 400, map[string]string{"error": "invalid pipeline query"})
			return
		}
		v, e := s.ListPipelines(r.Context(), in)
		respond(w, v, e)
	})
}
func registerPipelineTool(server *mcp.Server, service Service) {
	mcp.AddTool(server, &mcp.Tool{Name: "list_pipelines", Description: "Read time-ordered builds with their deployments, stage statuses and hints.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"product_id"}, "properties": map[string]any{"product_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "profile_id": map[string]any{"type": "string", "maxLength": 128}, "status": map[string]any{"type": "string", "enum": []string{"success", "running", "pending", "failed", "cancelled", "superseded", "attention", "skipped"}}, "environment_id": map[string]any{"type": "string", "maxLength": 128}, "q": map[string]any{"type": "string", "maxLength": 128}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "cursor": map[string]any{"type": "string", "maxLength": 2048}}}}, func(ctx context.Context, _ *mcp.CallToolRequest, in domain.ListPipelinesRequest) (*mcp.CallToolResult, any, error) {
		s, ok := service.(pipelineService)
		if !ok {
			return nil, nil, errors.New("pipeline service unavailable")
		}
		v, e := s.ListPipelines(ctx, in)
		return nil, v, e
	})
}
