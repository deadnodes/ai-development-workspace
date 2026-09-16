package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

// The provider retains external effects across application restarts. Its first
// GitOps response is lost after the mutation, exercising read-before-retry.
type persistedDeliveryFixture struct {
	dispatches, applies atomic.Int32
	applied             bool
}

var deliveryBase = strings.Repeat("a", 40)
var deliveryHead = strings.Repeat("b", 40)
var deliveryDigest = "sha256:" + strings.Repeat("d", 64)

func (f *persistedDeliveryFixture) TestConnection(context.Context, delivery.Connection) error {
	return nil
}
func (f *persistedDeliveryFixture) DiscoverRepositories(context.Context, delivery.Connection) ([]delivery.RepositoryInfo, error) {
	return []delivery.RepositoryInfo{{ID: 11, FullName: "team/source", DefaultBranch: "main", URL: "https://github.com/team/source", Private: true}, {ID: 22, FullName: "team/gitops", DefaultBranch: "main", URL: "https://github.com/team/gitops", Private: true}}, nil
}
func (f *persistedDeliveryFixture) Branches(context.Context, delivery.Connection, string) ([]delivery.BranchInfo, error) {
	return []delivery.BranchInfo{{Name: "main", SHA: deliveryBase}, {Name: "feature/work", SHA: deliveryHead}}, nil
}
func (f *persistedDeliveryFixture) Compare(context.Context, delivery.Connection, string, string, string) (delivery.CompareResult, error) {
	return delivery.CompareResult{BaseSHA: deliveryBase, HeadSHA: deliveryHead, Ahead: 1, Status: "ahead", Commits: []delivery.CommitInfo{{SHA: deliveryHead, Message: "Independent change"}}}, nil
}
func (f *persistedDeliveryFixture) Dispatch(_ context.Context, _ delivery.Connection, r delivery.BuildRequest) (delivery.DispatchResult, error) {
	f.dispatches.Add(1)
	if r.SourceSHA != deliveryHead || r.WorkflowSHA != deliveryBase {
		return delivery.DispatchResult{}, errors.New("incorrect workflow/source pin")
	}
	return delivery.DispatchResult{ProviderRunID: 77, Correlation: "rcp-" + r.OperationID}, nil
}
func (f *persistedDeliveryFixture) FindRun(_ context.Context, _ delivery.Connection, r delivery.BuildRequest) (delivery.RunResult, error) {
	return delivery.RunResult{Found: true, ID: 77, Status: "completed", Conclusion: "success", HeadSHA: deliveryBase, URL: "https://github.com/team/source/actions/runs/77", Artifacts: map[string]string{"operation_id": r.OperationID, "source_sha": deliveryHead, "image_repository": "ghcr.io/team/api", "image_tag": r.ImageTag, "digest": deliveryDigest, "available": "true", "observed_at": time.Now().UTC().Format(time.RFC3339Nano)}}, nil
}
func (f *persistedDeliveryFixture) InspectArtifact(_ context.Context, _ delivery.Connection, r delivery.ArtifactRequest) (delivery.ArtifactResult, error) {
	if r.EvidenceRunID == 0 {
		return delivery.ArtifactResult{Available: false, ObservedAt: time.Now().UTC()}, nil
	}
	if r.ExpectedDigest != deliveryDigest || r.SourceSHA != deliveryHead {
		return delivery.ArtifactResult{}, errors.New("inspection did not pin correlated build digest/source")
	}
	return delivery.ArtifactResult{Available: true, Digest: deliveryDigest, URI: "ghcr.io/team/api@" + deliveryDigest, ObservedAt: time.Now().UTC()}, nil
}
func (f *persistedDeliveryFixture) ReadGitOps(context.Context, delivery.Connection, delivery.GitOpsRequest) (delivery.GitOpsSnapshot, error) {
	r := delivery.GitOpsSnapshot{HeadSHA: strings.Repeat("c", 40), BlobSHA: strings.Repeat("e", 40), ImageRepository: "ghcr.io/team/api"}
	if f.applied {
		r.HeadSHA = strings.Repeat("f", 40)
		r.BlobSHA = strings.Repeat("1", 40)
		r.Digest = deliveryDigest
	}
	return r, nil
}
func (f *persistedDeliveryFixture) ApplyGitOps(_ context.Context, _ delivery.Connection, r delivery.GitOpsApply) (delivery.GitOpsResult, error) {
	f.applies.Add(1)
	if r.ExpectedHeadSHA != strings.Repeat("c", 40) || r.ExpectedBlobSHA != strings.Repeat("e", 40) || r.Digest != deliveryDigest {
		return delivery.GitOpsResult{}, errors.New("incorrect GitOps CAS")
	}
	f.applied = true
	return delivery.GitOpsResult{}, errors.New("simulated lost successful commit response")
}

func TestPostgresExternalRegistryAndDeliveryRestart(t *testing.T) {
	ctx := context.Background()
	db := isolatedDatabase(t)
	store, err := persistence.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	provider := &persistedDeliveryFixture{}
	service := application.NewWithProvider(store, provider)
	server := httptest.NewServer(transport.New(service, transport.Options{}))
	defer func() { server.Close() }()
	post := func(c domain.Command, want int) {
		t.Helper()
		c.Actor = "agent/persistence-test"
		if c.Data == nil {
			c.Data = map[string]any{}
		}
		body, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.Post(server.URL+"/api/commands", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s got%d want%d: %s", c.Action, res.StatusCode, want, out)
		}
	}
	for _, p := range []string{"p1", "p2"} {
		post(domain.Command{Action: "create_product", ID: p, Data: map[string]any{"name": p}}, 200)
	}
	post(domain.Command{Action: "create_github_connection", ID: "conn", Data: map[string]any{"name": "Shared installation", "app_id": 1, "installation_id": 2, "owner": "team", "private_key_ref": "env:RCP_TEST_KEY"}}, 200)
	for _, p := range []string{"p1", "p2"} {
		post(domain.Command{Action: "grant_connection", ProductID: p, Data: map[string]any{"connection_id": "conn"}}, 200)
	}
	res, err := http.Get(server.URL + "/api/connections/conn/repositories")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("repository discovery failed")
	}
	for _, r := range []struct{ id, product, name, role string }{{"source", "p1", "source", "SOURCE"}, {"gitops", "p1", "gitops", "GITOPS"}, {"shared-source", "p2", "source", "SOURCE"}} {
		post(domain.Command{Action: "import_repository", ID: r.id, ProductID: r.product, Data: map[string]any{"connection_id": "conn", "full_name": "team/" + r.name, "role": r.role, "default_branch": "main"}}, 200)
	}
	post(domain.Command{Action: "create_application", ID: "app", ProductID: "p1", Data: map[string]any{"name": "API", "repository_id": "source"}}, 200)
	post(domain.Command{Action: "create_environment", ID: "dev", ProductID: "p1", Data: map[string]any{"name": "Development", "cluster": "worker", "namespace": "app-dev"}}, 200)
	post(domain.Command{Action: "configure_component", ID: "build", ProductID: "p1", Data: map[string]any{"application_id": "app", "connection_id": "conn", "workflow": "build.yml", "workflow_ref": "main", "image_repository": "ghcr.io/team/api", "rebuild_missing": true}}, 200)
	post(domain.Command{Action: "configure_environment", ID: "target", ProductID: "p1", Data: map[string]any{"environment_id": "dev", "application_id": "app", "connection_id": "conn", "repository_id": "gitops", "purpose": "DEV", "ref": "main", "path": "deploy/api.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true}}, 200)
	post(domain.Command{Action: "create_feature", ID: "feature", ProductID: "p1", Data: map[string]any{"title": "Provider workflow", "problem": "Lost context", "goal": "Durable release intent", "repositories": []string{"source"}}}, 200)
	post(domain.Command{Action: "create_integration", ID: "integration", FeatureID: "feature", Data: map[string]any{"title": "API change", "objective": "Deploy independent code", "repositories": []string{"source"}, "branches": []domain.Branch{{RepositoryID: "source", Name: "feature/work"}}}}, 200)
	post(domain.Command{Action: "record_integration_revision", ID: "revision", IntegrationID: "integration", Data: map[string]any{"repository_id": "source", "branch": "feature/work", "base_commit": deliveryBase, "head_commit": deliveryHead, "commits": []string{deliveryHead}}}, 200)
	post(domain.Command{Action: "create_external_system", ID: "partner", Data: map[string]any{"name": "Payment partner", "team": "Billing", "contracts": []string{"Keep v1 compatible"}}}, 200)
	for _, p := range []string{"p1", "p2"} {
		post(domain.Command{Action: "create_system_relationship", ID: p + "-relation", ProductID: p, Data: map[string]any{"external_system_id": "partner", "type": "CONSUMES"}}, 200)
	}
	post(domain.Command{Action: "set_external_scope", ID: "feature-scope", FeatureID: "feature", Data: map[string]any{"relationship_ids": []string{"p1-relation"}}}, 200)
	post(domain.Command{Action: "set_external_scope", FeatureID: "feature", Data: map[string]any{"relationship_ids": []string{"p2-relation"}}}, 422)
	post(domain.Command{Action: "set_external_scope", ID: "integration-scope", IntegrationID: "integration", Data: map[string]any{"external_system_ids": []string{"partner"}}}, 200)
	post(domain.Command{Action: "create_gate", ID: "gate", FeatureID: "feature", Data: map[string]any{"title": "Contract check", "reason": "Partner compatibility", "integration_ids": []string{"integration"}}}, 200)
	post(domain.Command{Action: "set_external_scope", ID: "gate-scope", GateID: "gate", Data: map[string]any{"external_system_ids": []string{"partner"}}}, 200)
	post(domain.Command{Action: "deploy_integration", ID: "operation", IntegrationID: "integration", Data: map[string]any{"environment_id": "dev", "application_id": "app", "revision_id": "revision"}}, 200)
	tick := func() {
		t.Helper()
		if err := store.Update(ctx, func(st *domain.State) error {
			for i := range st.Operations {
				st.Operations[i].NextAttemptAt = time.Time{}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		worked, err := service.Tick(ctx)
		if err != nil || !worked {
			t.Fatalf("worker step worked=%v err=%v", worked, err)
		}
	}
	tick()
	tick()
	if provider.dispatches.Load() != 1 {
		t.Fatal("dispatch was not performed once")
	}
	// Restart after the dispatch intent/run correlation was persisted.
	server.Close()
	store.Close()
	store, err = persistence.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	service = application.NewWithProvider(store, provider)
	server = httptest.NewServer(transport.New(service, transport.Options{}))
	var state domain.State
	for i := 0; i < 10; i++ {
		state, err = service.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Operations) == 1 && state.Operations[0].Status == "SUCCEEDED" {
			break
		}
		tick()
	}
	state, err = service.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Operations) != 1 || state.Operations[0].Status != "SUCCEEDED" || state.Operations[0].DeploymentState != "GITOPS_APPLIED" {
		t.Fatalf("operation failed to reconcile: %+v", state.Operations)
	}
	if provider.dispatches.Load() != 1 || provider.applies.Load() != 1 {
		t.Fatalf("external effects duplicated dispatch=%d apply=%d", provider.dispatches.Load(), provider.applies.Load())
	}
	if len(state.GitHubConnections) != 1 || state.GitHubConnections[0].ProductID != "" || len(state.ConnectionGrants) != 2 || len(state.RegisteredRepositories) != 2 {
		t.Fatal("instance registry sharing lost")
	}
	attached := map[string]string{}
	for _, repo := range state.Repositories {
		attached[repo.ID] = repo.RegisteredRepositoryID
	}
	if attached["source"] == "" || attached["source"] != attached["shared-source"] {
		t.Fatal("product repository attachments did not share instance identity")
	}
	for _, repo := range state.RegisteredRepositories {
		if repo.ProductID != "" || repo.ProviderRepositoryID == 0 {
			t.Fatal("discovery lost numeric/provider instance identity")
		}
	}
	if len(state.ExternalSystems) != 1 || state.ExternalSystems[0].ProductID != "" || len(state.SystemRelationships) != 2 || len(state.ExternalScopes) != 3 {
		t.Fatal("external topology/history not preserved")
	}
	if len(state.OperationSteps) < 6 || len(state.DeliveryBuildRuns) == 0 || len(state.DeliveryArtifacts) == 0 {
		t.Fatalf("missing operation history steps=%d builds=%d artifacts=%d", len(state.OperationSteps), len(state.DeliveryBuildRuns), len(state.DeliveryArtifacts))
	}
	for _, run := range state.DeliveryBuildRuns {
		if run.Result.ID != 77 || run.Request.SourceSHA != deliveryHead || run.Request.WorkflowSHA != deliveryBase {
			t.Fatal("build observations lost provider identity or source/workflow pins")
		}
	}
	foundArtifact := false
	for _, a := range state.DeliveryArtifacts {
		if a.SourceCommit == deliveryHead && a.Digest == deliveryDigest {
			foundArtifact = true
		}
	}
	if !foundArtifact {
		t.Fatal("artifact source/digest provenance absent")
	}
	if len(state.Environments[0].Reconciled) != 0 || len(state.Environments[0].Runtime) != 0 {
		t.Fatal("GitOps completion invented Flux/runtime health")
	}
	// Final reopen compares every persisted record including append-only histories.
	before := state
	configurationBefore, err := service.Query(ctx, "get_product_configuration", "p1")
	if err != nil {
		t.Fatal(err)
	}
	server.Close()
	store.Close()
	store, err = persistence.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	service = application.NewWithProvider(store, provider)
	server = httptest.NewServer(transport.New(service, transport.Options{}))
	after, err := service.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("external state changed after reopen")
	}
	configurationAfter, err := service.Query(ctx, "get_product_configuration", "p1")
	if err != nil || !reflect.DeepEqual(configurationBefore, configurationAfter) {
		t.Fatalf("configuration mirror changed after database reopen: %v", err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "external-restart", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, call := range []struct {
		name     string
		args     map[string]any
		contains []string
	}{{"get_operation", map[string]any{"operation_id": "operation"}, []string{"GITOPS_APPLIED", "PENDING_RECONCILIATION", deliveryDigest}}, {"get_integration_context", map[string]any{"integration_id": "integration"}, []string{"Payment partner", "Keep v1 compatible", "feature-scope", "gate-scope"}}} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil || result.IsError {
			t.Fatalf("MCP %s: %v %+v", call.name, err, result)
		}
		body, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range call.contains {
			if !bytes.Contains(body, []byte(value)) {
				t.Error(fmt.Sprintf("%s context missing %s", call.name, value))
			}
		}
	}
}
