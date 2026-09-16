package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/domain"
)

// All semantic reads and mutations share the application layer with execute.
func registerQueryRoutes(mux *http.ServeMux, service Service) {
	for pattern, name := range map[string]string{
		"GET /api/features/{id}/graph":           "get_feature_graph",
		"GET /api/products/{id}/retention":       "get_artifact_retention",
		"GET /api/products/{id}/configuration":   "get_product_configuration",
		"GET /api/integrations/{id}/context":     "get_integration_context",
		"GET /api/integrations/{id}/git":         "get_integration_git",
		"GET /api/environments/{id}/state":       "get_environment_state",
		"GET /api/operations/{id}":               "get_operation",
		"GET /api/connections/{id}/repositories": "discover_repositories",
		"POST /api/connections/{id}/test":        "test_connection",
		"GET /api/repositories/{id}/branches":    "list_branches",
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			v, e := service.Query(r.Context(), name, r.PathValue("id"))
			respond(w, v, e)
		})
	}
	mux.HandleFunc("GET /api/attention", func(w http.ResponseWriter, r *http.Request) {
		v, e := service.Query(r.Context(), "list_attention", r.URL.Query().Get("product_id"))
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/integrations/{id}/deploy", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			write(w, 415, map[string]string{"error": "Use Content-Type: application/json"})
			return
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		var in deployInput
		if e := dec.Decode(&in); e != nil {
			write(w, 400, map[string]string{"error": "Invalid deployment JSON: " + e.Error()})
			return
		}
		if e := dec.Decode(new(any)); e != io.EOF {
			write(w, 400, map[string]string{"error": "Expected one JSON object"})
			return
		}
		in.IntegrationID = r.PathValue("id")
		v, e := service.Execute(r.Context(), in.command())
		respond(w, v, e)
	})
}

type deployInput struct {
	Actor          string `json:"actor" jsonschema:"Human or agent attribution"`
	IntegrationID  string `json:"integration_id" jsonschema:"Integration to deploy"`
	EnvironmentID  string `json:"environment_id" jsonschema:"Explicitly configured DEV environment"`
	ApplicationID  string `json:"application_id,omitempty" jsonschema:"Optional application selection when multiple components are configured"`
	RevisionID     string `json:"revision_id,omitempty" jsonschema:"Optional immutable revision; omitted resolves the bound source branch"`
	ExpectedDigest string `json:"expected_digest,omitempty" jsonschema:"Optional expected immutable artifact digest"`
}

func (in deployInput) command() domain.Command {
	data := map[string]any{"environment_id": in.EnvironmentID}
	if in.ApplicationID != "" {
		data["application_id"] = in.ApplicationID
	}
	if in.RevisionID != "" {
		data["revision_id"] = in.RevisionID
	}
	if in.ExpectedDigest != "" {
		data["expected_digest"] = in.ExpectedDigest
	}
	return domain.Command{Action: "deploy_integration", Actor: in.Actor, IntegrationID: in.IntegrationID, Data: data}
}

type deployExistingInput struct {
	Actor         string `json:"actor"`
	IntegrationID string `json:"integration_id"`
	EnvironmentID string `json:"environment_id"`
	ApplicationID string `json:"application_id"`
	RevisionID    string `json:"revision_id"`
	ArtifactID    string `json:"artifact_id"`
}

func (in deployExistingInput) command() domain.Command {
	return domain.Command{Action: "deploy_existing_artifact", Actor: in.Actor, IntegrationID: in.IntegrationID, Data: map[string]any{"environment_id": in.EnvironmentID, "application_id": in.ApplicationID, "revision_id": in.RevisionID, "artifact_id": in.ArtifactID}}
}
func registerProviderTools(server *mcp.Server, service Service) {
	registerBranchTools(server, service)
	mcp.AddTool(server, &mcp.Tool{Name: "get_feature_graph", Description: "Read a feature delivery graph: repository-scoped PR source/target branches, merge evidence, source revisions, compositions, artifacts and deployment observations. Historical reports are distinct from observed runtime; missing runtime is unknown."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		FeatureID string `json:"feature_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_feature_graph", in.FeatureID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "deploy_existing_artifact", Description: "Deploy a previously recorded successful build artifact to an enabled DEV GitOps mapping. Requires exact integration revision, fresh registry evidence and matching immutable digest. Never launches CI or rebuilds. Returns a persistent operation; GitOps success is not runtime health."}, func(ctx context.Context, _ *mcp.CallToolRequest, in deployExistingInput) (*mcp.CallToolResult, any, error) {
		v, e := service.Execute(ctx, in.command())
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_product_configuration", Description: "Export the effective product configuration from the authoritative database for a one-way Git mirror. Includes secret references, never secret resolution. This is not a backup or import format."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_product_configuration", in.ProductID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_integration_context", Description: "Read implementation context with provider observations and deployment operations for one integration."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		IntegrationID string `json:"integration_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_integration_context", in.IntegrationID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_git_state", Description: "Read observed Git branch/PR/check state. Unknown is not synchronized. Queue refresh_integration_git through execute to request a fresh observation."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		IntegrationID string `json:"integration_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_integration_git", in.IntegrationID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "refresh_environment_runtime", Description: "Queue read-only Kubernetes/Flux and GitOps/Actions inventory. Returns persistent operation immediately; never changes cluster resources or advances deployment readiness."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID     string `json:"product_id"`
		EnvironmentID string `json:"environment_id"`
		ApplicationID string `json:"application_id,omitempty"`
		Actor         string `json:"actor"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Execute(ctx, domain.Command{Action: "refresh_environment_runtime", ProductID: in.ProductID, Actor: in.Actor, Data: map[string]any{"environment_id": in.EnvironmentID, "application_id": in.ApplicationID}})
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_environment_state", Description: "Read desired GitOps state separately from reconciled and runtime observations."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		EnvironmentID string `json:"environment_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_environment_state", in.EnvironmentID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_operation", Description: "Read persisted operation status, step evidence and attention needs. Queued or pending does not mean deployed."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		OperationID string `json:"operation_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_operation", in.OperationID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_artifact_retention", Description: "Evaluate component retention against recorded availability; advisory only, never deletes artifacts."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_artifact_retention", in.ProductID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_attention_required", Description: "Read failed or blocked operations and other attention required for a product."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "list_attention", in.ProductID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "deploy_integration", Description: "Request a real DEV deployment operation for exact source. May dispatch CI and modify configured DEV GitOps files. Requires explicit environment deployment policy. Returns an operation to poll; success is not inferred from submission."}, func(ctx context.Context, _ *mcp.CallToolRequest, in deployInput) (*mcp.CallToolResult, any, error) {
		v, e := service.Execute(ctx, in.command())
		return nil, v, e
	})
}
