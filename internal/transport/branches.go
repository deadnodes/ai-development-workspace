package transport

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/domain"
)

type branchInput struct {
	Actor         string `json:"actor"`
	IntegrationID string `json:"integration_id"`
	ApplicationID string `json:"application_id"`
	BaseCommit    string `json:"base_commit,omitempty"`
	RevisionID    string `json:"revision_id,omitempty"`
	Approve       bool   `json:"approve"`
}

func registerBranchTools(server *mcp.Server, service Service) {
	for _, action := range []string{"create_integration_branch", "protect_integration_revision"} {
		mcp.AddTool(server, &mcp.Tool{Name: action, Description: "Explicit approved create-only source ref operation. Returns durable operation ID; no force updates. Pin references are application-managed, not administrator-proof GitHub branch protection."}, func(ctx context.Context, _ *mcp.CallToolRequest, in branchInput) (*mcp.CallToolResult, any, error) {
			v, e := service.Execute(ctx, domain.Command{Action: action, Actor: in.Actor, IntegrationID: in.IntegrationID, Data: map[string]any{"application_id": in.ApplicationID, "base_commit": in.BaseCommit, "revision_id": in.RevisionID, "approve": in.Approve}})
			return nil, v, e
		})
	}
}
