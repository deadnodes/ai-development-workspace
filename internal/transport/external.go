package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/domain"
)

// All semantic reads and mutations share the application layer with execute.
func registerQueryRoutes(mux routeRegistrar, service Service) {
	registerWorkRoutes(mux, service)
	for pattern, name := range map[string]string{
		"GET /api/execution-groups/{id}":              "get_execution_group",
		"GET /api/products/{id}/executions":           "list_executions",
		"GET /api/products/{id}/execution-events":     "list_execution_events",
		"GET /api/features/{id}/graph":                "get_feature_graph",
		"GET /api/products/{id}/retention":            "get_artifact_retention",
		"GET /api/products/{id}/configuration":        "get_product_configuration",
		"GET /api/integrations/{id}/context":          "get_integration_context",
		"GET /api/integrations/{id}/git":              "get_integration_git",
		"GET /api/products/{id}/unlinked-git-commits": "get_unlinked_git_commits",
		"GET /api/environments/{id}/state":            "get_environment_state",
		"GET /api/intelligence-tasks/{id}":            "get_intelligence_task",
		"GET /api/worker-jobs/{id}":                   "get_worker_job",
		"GET /api/operations/{id}":                    "get_operation",
		"GET /api/connections/{id}/repositories":      "discover_repositories",
		"POST /api/connections/{id}/test":             "test_connection",
		"GET /api/repositories/{id}/branches":         "list_branches",
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			queryID := r.PathValue("id")
			if name == "list_execution_events" {
				q := r.URL.Query()
				limit := 100
				if raw := q.Get("limit"); raw != "" {
					if parsed, err := strconv.Atoi(raw); err != nil {
						limit = -1
					} else {
						limit = parsed
					}
				}
				payload, _ := json.Marshal(map[string]any{"product_id": queryID, "cursor": q.Get("cursor"), "limit": limit, "unresolved_only": q.Get("unresolved_only") == "true", "due_only": q.Get("due_only") == "true"})
				queryID = string(payload)
			}
			v, e := service.Query(r.Context(), name, queryID)
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
	RequestKey     string `json:"request_key,omitempty"`
	Actor          string `json:"actor,omitempty" jsonschema:"Human or agent attribution"`
	IntegrationID  string `json:"integration_id" jsonschema:"Integration to deploy"`
	EnvironmentID  string `json:"environment_id" jsonschema:"Explicitly configured DEV environment"`
	ApplicationID  string `json:"application_id,omitempty" jsonschema:"Optional application selection when multiple components are configured"`
	RevisionID     string `json:"revision_id,omitempty" jsonschema:"Optional immutable revision; omitted resolves the bound source branch"`
	ExpectedDigest string `json:"expected_digest,omitempty" jsonschema:"Optional expected immutable artifact digest"`
}

func (in deployInput) command() domain.Command {
	data := map[string]any{"environment_id": in.EnvironmentID}
	if in.RequestKey != "" {
		data["request_key"] = in.RequestKey
	}
	if in.ApplicationID != "" {
		data["application_id"] = in.ApplicationID
	}
	if in.RevisionID != "" {
		data["revision_id"] = in.RevisionID
	}
	if in.ExpectedDigest != "" {
		data["expected_digest"] = in.ExpectedDigest
	}
	return domain.Command{Action: "deploy_integration", Actor: defaultActor(in.Actor), IntegrationID: in.IntegrationID, Data: data}
}

type deployExistingInput struct {
	Actor          string `json:"actor,omitempty"`
	IntegrationID  string `json:"integration_id"`
	EnvironmentID  string `json:"environment_id"`
	ApplicationID  string `json:"application_id"`
	RevisionID     string `json:"revision_id"`
	ArtifactID     string `json:"artifact_id"`
	RequestKey     string `json:"request_key,omitempty" jsonschema:"Optional stable request key for exact scoped replay"`
	ExpectedDigest string `json:"expected_digest,omitempty" jsonschema:"Optional expected immutable artifact digest"`
}

func (in deployExistingInput) command() domain.Command {
	data := map[string]any{"environment_id": in.EnvironmentID, "application_id": in.ApplicationID, "revision_id": in.RevisionID, "artifact_id": in.ArtifactID}
	if in.RequestKey != "" {
		data["request_key"] = in.RequestKey
	}
	if in.ExpectedDigest != "" {
		data["expected_digest"] = in.ExpectedDigest
	}
	return domain.Command{Action: "deploy_existing_artifact", Actor: defaultActor(in.Actor), IntegrationID: in.IntegrationID, Data: data}
}
func registerProviderTools(server *mcp.Server, service Service) {
	registerWorkTools(server, service)
	registerBranchTools(server, service)
	for _, action := range []string{"claim_execution", "heartbeat_execution", "report_execution", "release_execution", "takeover_execution", "set_desired_state", "create_execution_group", "add_execution_group_member", "remove_execution_group_member", "set_execution_group_harness", "set_product_stalled_timeout", "report_turn_end", "report_turn_start", "claim_execution_event_delivery", "ack_execution_event", "resolve_execution_event"} {
		registerExecutionCommandTool(server, service, action)
	}
	mcp.AddTool(server, &mcp.Tool{Name: "set_product_idle_policy", Description: "Set group idle and member staleness thresholds in seconds (0 disables, maximum 604800). Product owner or instance admin only."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID                   string `json:"product_id"`
		IdleAfterSeconds            int    `json:"idle_after_seconds"`
		WaitTimeoutSeconds          int    `json:"wait_timeout_seconds"`
		OwnerBlockedIntervalSeconds int    `json:"owner_blocked_interval_seconds"`
		MemberStaleAfterSeconds      int    `json:"member_stale_after_seconds"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Execute(ctx, domain.Command{Action: "set_product_idle_policy", Actor: "agent", ProductID: in.ProductID, Data: map[string]any{
			"idle_after_seconds": in.IdleAfterSeconds, "wait_timeout_seconds": in.WaitTimeoutSeconds, "owner_blocked_interval_seconds": in.OwnerBlockedIntervalSeconds,
			"member_stale_after_seconds": in.MemberStaleAfterSeconds,
		}})
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_execution_group", Description: "Read owner-scoped execution group members, their current state and next actions, and the idle evaluation."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		GroupID string `json:"group_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_execution_group", in.GroupID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "list_executions", Description: "List Product scoped Integration executions."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "list_executions", in.ProductID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "list_execution_events", Description: "List Product scoped execution events with a stable created_at,id cursor and delivery filters."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID      string `json:"product_id"`
		Cursor         string `json:"cursor,omitempty"`
		Limit          int    `json:"limit,omitempty"`
		UnresolvedOnly bool   `json:"unresolved_only,omitempty"`
		DueOnly        bool   `json:"due_only,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		payload, _ := json.Marshal(map[string]any{"product_id": in.ProductID, "cursor": in.Cursor, "limit": in.Limit, "unresolved_only": in.UnresolvedOnly, "due_only": in.DueOnly})
		v, e := service.Query(ctx, "list_execution_events", string(payload))
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_intelligence_task", Description: "Read a scoped generated draft or bounded conflict patch with provenance, stale-input state and mandatory review. Does not publish, merge or deploy."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		TaskID string `json:"task_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_intelligence_task", in.TaskID)
		return nil, v, e
	})
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
	mcp.AddTool(server, &mcp.Tool{Name: "get_unlinked_git_commits", Description: "Read commits discovered in attached repositories that are not linked to a Feature or Integration. Ask the agent to attach them."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID string `json:"product_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_unlinked_git_commits", in.ProductID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "refresh_environment_runtime", Description: "Queue read-only Kubernetes/Flux and GitOps/Actions inventory. Returns persistent operation immediately; never changes cluster resources or advances deployment readiness."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID     string `json:"product_id"`
		EnvironmentID string `json:"environment_id"`
		ApplicationID string `json:"application_id,omitempty"`
		Actor         string `json:"actor,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Execute(ctx, domain.Command{Action: "refresh_environment_runtime", ProductID: in.ProductID, Actor: defaultActor(in.Actor), Data: map[string]any{"environment_id": in.EnvironmentID, "application_id": in.ApplicationID}})
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_environment_state", Description: "Read desired GitOps state separately from reconciled and runtime observations."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		EnvironmentID string `json:"environment_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_environment_state", in.EnvironmentID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_worker_job", Description: "Read one bounded worker job and its attempts; no global state required."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		JobID string `json:"job_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_worker_job", in.JobID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_operation", Description: "Read persisted operation status, step evidence and attention needs. agent_action RESOLVE_CONFLICT returns exact frozen branches/commits and the existing resolution workflow for the requesting agent; RCP does not launch another agent. UNAVAILABLE requires context reconciliation, not a partial resolution. Reconcile is a separate authorized build/GitOps request. Queued or pending does not mean deployed."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		OperationID string `json:"operation_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Query(ctx, "get_operation", in.OperationID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_artifact_retention", Description: "Read the normalized protected-root inventory, effective minimum-10 policies, recorded availability/freshness and blockers. Pure stored evidence; no registry fetch. deletion_allowed is always false and InputHash is not deletion authority."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
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

func registerExecutionCommandTool(server *mcp.Server, service Service, action string) {
	description := "Record an execution lifecycle command using the shared application policy. Fence and lease times are server managed."
	if action == "report_turn_end" {
		description = "Report a harness turn end. Provide harness_kind and at least one of session_ref or tmux_target; matching uses either supplied identifier."
	} else if action == "report_turn_start" {
		description = "Optionally report a registered group orchestrator turn start. Provide harness_kind and session_ref or tmux_target; turn_id is recommended."
	}
	mcp.AddTool(server, &mcp.Tool{Name: action, Description: description}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Actor         string         `json:"actor,omitempty"`
		ProductID     string         `json:"product_id,omitempty"`
		IntegrationID string         `json:"integration_id,omitempty"`
		Data          map[string]any `json:"data"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Execute(ctx, domain.Command{Action: action, Actor: defaultActor(in.Actor), ProductID: in.ProductID, IntegrationID: in.IntegrationID, Data: in.Data})
		return nil, v, e
	})
}
