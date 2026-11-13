package transport

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"

	"releasecontrol/internal/domain"
	"strings"
)

type deploymentService interface {
	DeployBuild(context.Context, domain.DeployBuildRequest) (domain.Deployment, error)
	GetDeployment(context.Context, domain.DeploymentLookup) (domain.Deployment, error)
	ListDeployments(context.Context, domain.ListDeploymentsRequest) (domain.DeploymentPage, error)
}

func registerDeploymentRoutes(mux *http.ServeMux, service Service) {
	decode := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			write(w, 415, map[string]string{"error": "application/json required"})
			return false
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		d.DisallowUnknownFields()
		if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
			write(w, 400, map[string]string{"error": "invalid deployment request"})
			return false
		}
		return true
	}
	get := func(w http.ResponseWriter) (deploymentService, bool) {
		s, ok := service.(deploymentService)
		if !ok {
			write(w, 501, map[string]string{"error": "deployment service unavailable"})
		}
		return s, ok
	}
	mux.HandleFunc("POST /api/deployments", func(w http.ResponseWriter, r *http.Request) {
		s, ok := get(w)
		if !ok {
			return
		}
		var in domain.DeployBuildRequest
		if !decode(w, r, &in) {
			return
		}
		v, e := s.DeployBuild(r.Context(), in)
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/deployments/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := get(w)
		if !ok {
			return
		}
		v, e := s.GetDeployment(r.Context(), domain.DeploymentLookup{ProductID: r.URL.Query().Get("product_id"), DeploymentID: r.PathValue("id")})
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/deployments", func(w http.ResponseWriter, r *http.Request) {
		s, ok := get(w)
		if !ok {
			return
		}
		values, e := strconvQueryHistory(r)
		if e != nil {
			write(w, 400, map[string]string{"error": "invalid deployment query"})
			return
		}
		raw, _ := json.Marshal(values)
		var in domain.ListDeploymentsRequest
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil {
			write(w, 400, map[string]string{"error": "invalid deployment query"})
			return
		}
		v, e := s.ListDeployments(r.Context(), in)
		respond(w, v, e)
	})
}
func registerDeploymentTools(server *mcp.Server, service Service) {
	unavailable := errors.New("deployment service unavailable")
	mcp.AddTool(server, deploymentTool("deploy_build", "Deploy a finished build to Dev only after CI and registry verification. RCP opens and squash-merges its GitHub Flux PR with a SHA lock. Production rejected. Only active deployments deduplicate by environment, profile and image digests; terminal matches create a new Deployment, already_applied without a PR when Flux has the values. Existing request_key replay returns its historical handle; rebinding conflicts. Returns a durable Deployment; uncertainty reconciles without blindly merging again."), func(ctx context.Context, _ *mcp.CallToolRequest, in domain.DeployBuildRequest) (*mcp.CallToolResult, any, error) {
		s, ok := service.(deploymentService)
		if !ok {
			return nil, nil, unavailable
		}
		v, e := s.DeployBuild(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(server, deploymentTool("get_deployment", "Read one Product-scoped Deployment including CI and registry evidence, PR and merge SHA. GitOps completion does not claim runtime health."), func(ctx context.Context, _ *mcp.CallToolRequest, in domain.DeploymentLookup) (*mcp.CallToolResult, any, error) {
		s, ok := service.(deploymentService)
		if !ok {
			return nil, nil, unavailable
		}
		v, e := s.GetDeployment(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(server, deploymentTool("list_deployments", "List Product-scoped Dev Deployments with optional environment/profile filters and a scope-bound cursor. Default 25, maximum 100."), func(ctx context.Context, _ *mcp.CallToolRequest, in domain.ListDeploymentsRequest) (*mcp.CallToolResult, any, error) {
		s, ok := service.(deploymentService)
		if !ok {
			return nil, nil, unavailable
		}
		v, e := s.ListDeployments(ctx, in)
		return nil, v, e
	})
}
func deploymentTool(name, description string) *mcp.Tool {
	fields := map[string]any{}
	str := func(max int) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "maxLength": max}
	}
	fields["product_id"] = str(128)
	required := []string{"product_id"}
	switch name {
	case "deploy_build":
		fields["build_id"] = str(128)
		fields["environment_id"] = map[string]any{"type": "string", "enum": []string{"dev"}}
		fields["request_key"] = str(128)
		required = append(required, "build_id", "environment_id")
	case "get_deployment":
		fields["deployment_id"] = str(128)
		required = append(required, "deployment_id")
	case "list_deployments":
		fields["profile_id"] = str(128)
		fields["environment_id"] = map[string]any{"type": "string", "enum": []string{"dev"}}
		fields["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
		fields["cursor"] = str(2048)
	}
	return &mcp.Tool{Name: name, Description: description, InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": fields}}
}
