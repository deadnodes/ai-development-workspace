package transport

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"strings"
)

type buildService interface {
	QueueBuild(context.Context, domain.QueueBuildRequest) (domain.Build, error)
	GetBuild(context.Context, domain.BuildLookup) (domain.Build, error)
	ListBuilds(context.Context, domain.ListBuildsRequest) (domain.BuildPage, error)
	CancelBuild(context.Context, domain.BuildLookup) (domain.Build, error)
	ListBuildProfiles(context.Context, string) ([]buildconfig.Profile, error)
}

func registerBuildRoutes(mux *http.ServeMux, service Service) {
	decode := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			write(w, 415, map[string]string{"error": "application/json required"})
			return false
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		d.DisallowUnknownFields()
		if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
			write(w, 400, map[string]string{"error": "invalid build request"})
			return false
		}
		return true
	}
	get := func(w http.ResponseWriter) (buildService, bool) {
		s, ok := service.(buildService)
		if !ok {
			write(w, 501, map[string]string{"error": "build service unavailable"})
		}
		return s, ok
	}
	mux.HandleFunc("POST /api/builds", func(w http.ResponseWriter, r *http.Request) {
		s, ok := get(w)
		if !ok {
			return
		}
		var in domain.QueueBuildRequest
		if !decode(w, r, &in) {
			return
		}
		v, e := s.QueueBuild(r.Context(), in)
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/builds/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := get(w)
		if !ok {
			return
		}
		v, e := s.GetBuild(r.Context(), domain.BuildLookup{ProductID: r.URL.Query().Get("product_id"), BuildID: r.PathValue("id")})
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/builds/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		s, ok := get(w)
		if !ok {
			return
		}
		var in struct {
			ProductID string `json:"product_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		v, e := s.CancelBuild(r.Context(), domain.BuildLookup{ProductID: in.ProductID, BuildID: r.PathValue("id")})
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/builds", func(w http.ResponseWriter, r *http.Request) {
		s, ok := get(w)
		if !ok {
			return
		}
		values, e := strconvQueryHistory(r)
		if e != nil {
			write(w, 400, map[string]string{"error": "invalid build query"})
			return
		}
		raw, _ := json.Marshal(values)
		var in domain.ListBuildsRequest
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil {
			write(w, 400, map[string]string{"error": "invalid build query"})
			return
		}
		v, e := s.ListBuilds(r.Context(), in)
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/build-profiles", func(w http.ResponseWriter, r *http.Request) {
		s, ok := get(w)
		if !ok {
			return
		}
		v, e := s.ListBuildProfiles(r.Context(), r.URL.Query().Get("product_id"))
		respond(w, v, e)
	})
}
func registerBuildTools(server *mcp.Server, service Service) {
	unavailable := errors.New("build service unavailable")
	mcp.AddTool(server, buildTool("queue_build", "Queue an idempotent image-only Forgejo build from branch HEAD or a verified commit SHA (mutually exclusive; neither uses the default branch). No deployment. Returns a durable Build handle; inspect get_build, never re-dispatch an uncertain build."), func(ctx context.Context, _ *mcp.CallToolRequest, in domain.QueueBuildRequest) (*mcp.CallToolResult, any, error) {
		s, ok := service.(buildService)
		if !ok {
			return nil, nil, unavailable
		}
		v, e := s.QueueBuild(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(server, buildTool("get_build", "Read one Product-scoped durable Build, source SHA, Forgejo run and registry-verified image digest."), func(ctx context.Context, _ *mcp.CallToolRequest, in domain.BuildLookup) (*mcp.CallToolResult, any, error) {
		s, ok := service.(buildService)
		if !ok {
			return nil, nil, unavailable
		}
		v, e := s.GetBuild(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(server, buildTool("cancel_build", "Cancel a QUEUED build only. Running or uncertain Forgejo execution cannot be cancelled here."), func(ctx context.Context, _ *mcp.CallToolRequest, in domain.BuildLookup) (*mcp.CallToolResult, any, error) {
		s, ok := service.(buildService)
		if !ok {
			return nil, nil, unavailable
		}
		v, e := s.CancelBuild(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(server, buildTool("list_builds", "List Product-scoped Builds, optional profile/status filter and scope-bound cursor. Default 25, maximum 100."), func(ctx context.Context, _ *mcp.CallToolRequest, in domain.ListBuildsRequest) (*mcp.CallToolResult, any, error) {
		s, ok := service.(buildService)
		if !ok {
			return nil, nil, unavailable
		}
		v, e := s.ListBuilds(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(server, buildTool("list_build_profiles", "List operator-configured Build profiles for a Product, without credentials. Image builds only; no deployment."), func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
	}) (*mcp.CallToolResult, any, error) {
		s, ok := service.(buildService)
		if !ok {
			return nil, nil, unavailable
		}
		v, e := s.ListBuildProfiles(ctx, in.ProductID)
		return nil, v, e
	})
}

// Explicit schemas bound selectors before they reach the shared application service.
func buildTool(name, description string) *mcp.Tool {
	fields := map[string]any{}
	str := func(max int) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "maxLength": max}
	}
	fields["product_id"] = str(128)
	required := []string{"product_id"}
	switch name {
	case "queue_build":
		fields["profile_id"] = str(128)
		fields["branch"] = str(255)
		fields["sha"] = map[string]any{"type": "string", "pattern": "^[0-9a-f]{40}$"}
		fields["request_key"] = str(128)
		required = append(required, "profile_id")
	case "get_build", "cancel_build":
		fields["build_id"] = str(128)
		required = append(required, "build_id")
	case "list_builds":
		fields["profile_id"] = str(128)
		fields["status"] = map[string]any{"type": "string", "enum": []string{"QUEUED", "DISPATCHED", "RUNNING", "SUCCEEDED", "FAILED", "CANCELLED", "RECONCILIATION_REQUIRED"}}
		fields["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
		fields["cursor"] = str(2048)
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": fields}
	if name == "queue_build" {
		schema["not"] = map[string]any{"required": []string{"branch", "sha"}}
	}
	return &mcp.Tool{Name: name, Description: description, InputSchema: schema}
}
