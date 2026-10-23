package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

func TestCallerConflictActionRealHTTPAndMCP(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var store interface {
				application.Store
				Close()
			}
			var err error
			if kind == "local" {
				store, err = persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "caller.db"))
			} else {
				store, err = persistence.Open(ctx, isolatedDatabase(t))
			}
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			st := domain.EmptyState()
			st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "caller-p"}, Name: "Caller test"})
			st.Repositories = append(st.Repositories, domain.Repository{Meta: domain.Meta{ID: "caller-repo", ProductID: "caller-p"}, Name: "owner/repo", URL: "https://github.com/owner/repo", Provider: "github", Role: "APPLICATION"})
			env := domain.Environment{Meta: domain.Meta{ID: "caller-dev", ProductID: "caller-p"}, Name: "Dev"}
			st.Environments = append(st.Environments, env)
			app := domain.Application{Meta: domain.Meta{ID: "caller-app", ProductID: "caller-p"}, Name: "API", RepositoryID: "caller-repo", Kind: "APPLICATION"}
			st.Applications = append(st.Applications, app)
			base := strings.Repeat("a", 40)
			heads := []string{strings.Repeat("b", 40), strings.Repeat("c", 40)}
			for n, side := range []string{"a", "b"} {
				f := domain.Feature{Meta: domain.Meta{ID: "caller-f" + side, ProductID: "caller-p"}, Title: side, Goal: "Preserve " + side, Status: "active"}
				in := domain.Integration{Meta: domain.Meta{ID: "caller-i" + side, ProductID: "caller-p", FeatureID: f.ID}, Title: side, Objective: "Keep " + side, Status: "working"}
				st.Features = append(st.Features, f)
				st.Integrations = append(st.Integrations, in)
				st.IntegrationRevisions = append(st.IntegrationRevisions, domain.IntegrationRevision{Meta: domain.Meta{ID: "caller-r" + side, ProductID: "caller-p", FeatureID: f.ID}, IntegrationID: in.ID, RepositoryID: "caller-repo", Branch: "feature/" + side, BaseCommit: base, HeadCommit: heads[n], Commits: []string{heads[n]}})
			}
			comp := domain.Composition{Meta: domain.Meta{ID: "caller-comp", ProductID: "caller-p"}, EnvironmentID: env.ID, Name: "A+B", Status: "planned", EnvironmentSnapshot: env, ApplicationSnapshots: []domain.Application{app}, RevisionSnapshots: append([]domain.IntegrationRevision{}, st.IntegrationRevisions...), Components: []domain.CompositionComponent{{ApplicationID: app.ID, BaseRef: "main", BaseCommit: base, TargetBranch: "generated/caller", RevisionIDs: []string{"caller-ra", "caller-rb"}}}}
			st.Compositions = append(st.Compositions, comp)
			source := domain.FlowSource{RepositoryID: "caller-repo", Plan: delivery.SourcePlan{Repository: "owner/repo", BaseRef: "main", BaseSHA: base, TargetBranch: "generated/caller", OperationID: "caller-op", HeadSHAs: heads}, Result: &delivery.SourceResult{Branch: "generated/caller", Conflict: true, Detail: "conflict"}}
			st.Operations = append(st.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "caller-op", ProductID: "caller-p"}, Kind: "COMPOSE", Status: "FAILED", DeploymentState: "BLOCKED", Phase: "COMPOSE_SOURCE", EnvironmentID: env.ID, IntegrationIDs: []string{"caller-ia", "caller-ib"}, CompositionSnapshot: &comp, Sources: []domain.FlowSource{source}})
			st.CompositionConflicts = append(st.CompositionConflicts, domain.CompositionConflict{Meta: domain.Meta{ID: "caller-conflict", ProductID: "caller-p"}, OperationID: "caller-op", CompositionID: comp.ID, RepositoryID: source.RepositoryID, Source: source, Features: append([]domain.Feature{}, st.Features...), Integrations: append([]domain.Integration{}, st.Integrations...), Status: "requires_resolution"})
			if err := store.Update(ctx, func(target *domain.State) error { *target = st; return nil }); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(transport.New(application.New(store), transport.Options{}))
			defer server.Close()
			r, err := http.Get(server.URL + "/api/operations/caller-op")
			if err != nil {
				t.Fatal(err)
			}
			var httpReply map[string]json.RawMessage
			err = json.NewDecoder(r.Body).Decode(&httpReply)
			r.Body.Close()
			if err != nil || r.StatusCode != http.StatusOK {
				t.Fatalf("HTTP reply: %d %v", r.StatusCode, err)
			}
			var action struct {
				Type, Status string
				Conflicts    []struct {
					Inputs []struct {
						OriginBranch string `json:"origin_branch"`
						HeadSHA      string `json:"head_sha"`
					}
				}
			}
			if err := json.Unmarshal(httpReply["agent_action"], &action); err != nil || action.Type != "RESOLVE_CONFLICT" || action.Status != "ACTION_REQUIRED" || len(action.Conflicts) != 1 || len(action.Conflicts[0].Inputs) != 2 || action.Conflicts[0].Inputs[0].OriginBranch != "feature/a" || action.Conflicts[0].Inputs[1].HeadSHA != heads[1] {
				t.Fatalf("caller did not receive branches/SHAs: %s %v", httpReply["agent_action"], err)
			}
			session, err := mcp.NewClient(&mcp.Implementation{Name: "caller-resolution-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_operation", Arguments: map[string]any{"operation_id": "caller-op"}})
			if err != nil || result.IsError {
				t.Fatalf("MCP get_operation: %v %+v", err, result)
			}
			b, _ := json.Marshal(result.StructuredContent)
			var mcpReply map[string]json.RawMessage
			if err := json.Unmarshal(b, &mcpReply); err != nil {
				t.Fatal(err)
			}
			var fromHTTP, fromMCP any
			_ = json.Unmarshal(httpReply["agent_action"], &fromHTTP)
			_ = json.Unmarshal(mcpReply["agent_action"], &fromMCP)
			if !reflect.DeepEqual(fromHTTP, fromMCP) {
				t.Fatal("HTTP and actual MCP SDK caller tasks differ")
			}
		})
	}
}
