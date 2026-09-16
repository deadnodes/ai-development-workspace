package transport

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"releasecontrol/internal/application"
	"releasecontrol/internal/workspace"
)

type localGitService interface {
	LocalGitState(context.Context, string, string) (workspace.GitState, error)
	PlanLocalGitSync(context.Context, application.LocalGitRequest) (application.LocalGitPlan, error)
	RecordLocalGitSync(context.Context, application.LocalGitRequest) (any, error)
}

func registerLocalGitRoutes(mux *http.ServeMux, service Service) {
	s, ok := service.(localGitService)
	if !ok {
		return
	}
	mux.HandleFunc("GET /api/products/{id}/local-checkouts/{checkout}/git", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.LocalGitState(r.Context(), r.PathValue("id"), r.PathValue("checkout"))
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/local-git/plan", func(w http.ResponseWriter, r *http.Request) {
		var in application.LocalGitRequest
		if !workspaceJSON(w, r, &in) {
			return
		}
		v, e := s.PlanLocalGitSync(r.Context(), in)
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/local-git/record", func(w http.ResponseWriter, r *http.Request) {
		var in application.LocalGitRequest
		if !workspaceJSON(w, r, &in) {
			return
		}
		v, e := s.RecordLocalGitSync(r.Context(), in)
		respond(w, v, e)
	})
}
func registerLocalGitTools(server *mcp.Server, service Service) {
	s, ok := service.(localGitService)
	if !ok {
		return
	}
	mcp.AddTool(server, &mcp.Tool{Name: "get_local_git_state", Description: "Read real local checkout branch/HEAD, dirty state, cached origin upstream SHA and ahead/behind. Checkout must belong to Product and configured workspace. Does not fetch; cached refs are not live remote evidence."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID  string `json:"product_id"`
		CheckoutID string `json:"checkout_id"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := s.LocalGitState(ctx, in.ProductID, in.CheckoutID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "plan_local_git_sync", Description: "Persist a FETCH or FAST_FORWARD plan for an external host agent using its own credentials. Requires exact expected_head. Never executes Git or edits source. FAST_FORWARD requires clean attached branch strictly behind cached origin. Agent must review trusted Git configuration, preserve guards and record observation afterward."}, func(ctx context.Context, _ *mcp.CallToolRequest, in application.LocalGitRequest) (*mcp.CallToolResult, any, error) {
		v, e := s.PlanLocalGitSync(ctx, in)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "record_local_git_sync", Description: "Re-read local Git state and append observed result of an external-agent plan. FAST_FORWARD must match exact planned target/branch and remain clean. FETCH is observation only, not proof of network execution. Does not execute Git mutations."}, func(ctx context.Context, _ *mcp.CallToolRequest, in application.LocalGitRequest) (*mcp.CallToolResult, any, error) {
		v, e := s.RecordLocalGitSync(ctx, in)
		return nil, v, e
	})
}
