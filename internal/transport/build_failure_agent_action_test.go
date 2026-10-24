package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerconfig"
)

// A seeded historical chain tests wire serialization and reopen. Normal job
// admission/reporting is tested separately in application and real Git fixtures.
func buildFailureWireFixture() domain.State {
	st := domain.EmptyState()
	st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "Wire fixture"}}
	env := domain.Environment{Meta: domain.Meta{ID: "env", ProductID: "p"}, Name: "Dev"}
	app := domain.Application{Meta: domain.Meta{ID: "app", ProductID: "p"}, RepositoryID: "repo", Name: "API", Kind: "APPLICATION"}
	st.Environments, st.Applications = []domain.Environment{env}, []domain.Application{app}
	st.Repositories = []domain.Repository{{Meta: domain.Meta{ID: "repo", ProductID: "p"}, Name: "owner/repo", URL: "https://github.com/owner/repo", Role: "APPLICATION", Provider: "github"}}
	base, merged := strings.Repeat("a", 40), strings.Repeat("d", 40)
	heads := []string{strings.Repeat("b", 40), strings.Repeat("c", 40)}
	for n, side := range []string{"a", "b"} {
		st.Features = append(st.Features, domain.Feature{Meta: domain.Meta{ID: "f" + side, ProductID: "p"}, Title: side, Goal: "CURRENT_PRIVATE_CONTEXT", Status: "active"})
		st.Integrations = append(st.Integrations, domain.Integration{Meta: domain.Meta{ID: "i" + side, ProductID: "p", FeatureID: "f" + side}, Title: side, Status: "working"})
		st.IntegrationRevisions = append(st.IntegrationRevisions, domain.IntegrationRevision{Meta: domain.Meta{ID: "r" + side, ProductID: "p", FeatureID: "f" + side}, IntegrationID: "i" + side, RepositoryID: "repo", Branch: "feature/" + side, BaseCommit: base, HeadCommit: heads[n]})
	}
	comp := domain.Composition{Meta: domain.Meta{ID: "comp", ProductID: "p"}, Status: "planned", EnvironmentID: "env", EnvironmentSnapshot: env, ApplicationSnapshots: []domain.Application{app}, RevisionSnapshots: st.IntegrationRevisions, Components: []domain.CompositionComponent{{ApplicationID: "app", BaseRef: "main", BaseCommit: base, TargetBranch: "generated/source", RevisionIDs: []string{"ra", "rb"}}}}
	source := domain.FlowSource{RepositoryID: "repo", Plan: delivery.SourcePlan{Repository: "owner/repo", OperationID: "parent", BaseRef: "main", BaseSHA: base, TargetBranch: "generated/source", HeadSHAs: heads}, Result: &delivery.SourceResult{Branch: "generated/source", SHA: merged}}
	native := domain.NativeDelivery{ConfigRevision: "cfg", BuildRecipeRevision: "v1", BuildRecipeHash: workerconfig.Fingerprint("recipe"), PublicationRecipeRevision: "v1", PublicationRecipeHash: workerconfig.Fingerprint("publication"), Deadline: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)}
	prepared := domain.DeliverySnapshot{Application: app, Environment: env, Native: &native, Build: domain.ComponentBuild{ApplicationID: "app", Mode: "NATIVE", Native: &domain.NativeBinding{BuildRecipeID: "recipe", PublicationRecipeID: "publish", Parameters: map[string]any{}}}}
	parent := domain.ExternalOperation{Meta: domain.Meta{ID: "parent", ProductID: "p"}, Kind: "COMPOSE", Status: "FAILED", EnvironmentID: "env", IntegrationIDs: []string{"ia", "ib"}, CompositionSnapshot: &comp, Sources: []domain.FlowSource{source}, PreparedDeployments: []domain.DeliverySnapshot{prepared}, ChildIDs: []string{"delivery"}}
	parentHash := workerconfig.Fingerprint(struct {
		Composition *domain.Composition
		Sources     []domain.FlowSource
	}{parent.CompositionSnapshot, parent.Sources})
	snapshot := prepared
	snapshot.Revision = domain.IntegrationRevision{Meta: domain.Meta{ID: "generated-revision", ProductID: "p"}, RepositoryID: "repo", Branch: "generated/source", BaseCommit: base, HeadCommit: merged}
	childNative := native
	childNative.ParentHash, childNative.BuildJobID = parentHash, "job"
	child := domain.ExternalOperation{Meta: domain.Meta{ID: "delivery", ProductID: "p"}, Kind: "DEPLOY", ParentID: "parent", Status: "FAILED", EnvironmentID: "env", ApplicationID: "app", Snapshot: &snapshot, Native: &childNative}
	inputs := domain.WorkerInputs{RecipeID: "recipe", RecipeRevision: "v1", RecipeHash: native.BuildRecipeHash, ConfigRevision: "cfg", RepositoryID: "repo", RepositoryURL: "https://github.com/owner/repo", RevisionID: snapshot.Revision.ID, SourceSHA: merged, Parameters: map[string]any{}, Execution: &domain.WorkerExecution{Kind: "local_oci_build"}, SourceProof: &domain.WorkerSourceProof{DeliveryID: "delivery", ParentID: "parent", ParentHash: parentHash, SnapshotHash: workerconfig.Fingerprint(&snapshot)}}
	hash := workerconfig.Fingerprint(inputs)
	job := domain.WorkerJob{Meta: domain.Meta{ID: "job", ProductID: "p"}, OperationID: "worker-op", Inputs: inputs, Capability: "local_oci_build", Status: "FAILED", Fence: 1, InputHash: hash, RequestHash: hash}
	report := domain.WorkerReport{WorkerAttemptRequest: domain.WorkerAttemptRequest{JobID: "job", AttemptID: "attempt", Fence: 1, InputHash: hash}, Result: "CHECK_FAILED"}
	st.Operations = []domain.ExternalOperation{parent, child, {Meta: domain.Meta{ID: "worker-op", ProductID: "p"}, Kind: domain.WorkerOperationKind, ParentID: "delivery", RepositoryID: "repo", Status: "FAILED", Attempts: 1}}
	st.WorkerJobs = []domain.WorkerJob{job}
	st.Events = []domain.Event{{ID: "report-audit", Action: "worker_report", EntityID: job.ID, ProductID: job.ProductID, Data: domain.Command{Action: "worker_report", ID: job.ID, ProductID: job.ProductID, Data: map[string]any{"status": job.Status, "fence": job.Fence, "input_hash": job.InputHash}}}}
	st.WorkerAttempts = []domain.WorkerAttempt{{Meta: domain.Meta{ID: "attempt", ProductID: "p"}, JobID: "job", Status: "FAILED", Fence: 1, InputHash: hash, Report: &report, Principal: "PRIVATE_WORKER_PRINCIPAL"}}
	return st
}

func TestBuildFailureAgentHTTPMCPAndPersistentReopen(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "build-action.db")
			dsn := ""
			if kind == "postgres" {
				dsn = isolatedDatabase(t)
			}
			type storeType interface {
				application.Store
				Close()
			}
			open := func() storeType {
				if kind == "postgres" {
					s, err := persistence.Open(ctx, dsn)
					if err != nil {
						t.Fatal(err)
					}
					return s
				}
				s, err := persistence.OpenLocal(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			store := open()
			t.Cleanup(func() { store.Close() })
			if err := store.Update(ctx, func(st *domain.State) error { *st = buildFailureWireFixture(); return nil }); err != nil {
				t.Fatal(err)
			}
			var originalAction string
			for phase := range 2 {
				if phase == 1 {
					store.Close()
					store = open()
				}
				func() {
					before, _ := store.Read(ctx)
					beforeJSON, _ := json.Marshal(before)
					server := httptest.NewServer(transport.New(application.New(store), transport.Options{}))
					defer server.Close()
					res, err := http.Get(server.URL + "/api/operations/parent")
					if err != nil {
						t.Fatal(err)
					}
					var httpReply map[string]json.RawMessage
					err = json.NewDecoder(res.Body).Decode(&httpReply)
					res.Body.Close()
					if err != nil || res.StatusCode != http.StatusOK {
						t.Fatal("HTTP", res.StatusCode, err)
					}
					var action struct {
						Type     string `json:"type"`
						Status   string `json:"status"`
						Failures []struct {
							JobID          string `json:"job_id"`
							AttemptID      string `json:"attempt_id"`
							ReportedResult string `json:"reported_result"`
						} `json:"failures"`
					}
					if err := json.Unmarshal(httpReply["agent_action"], &action); err != nil || action.Type != "INVESTIGATE_BUILD_FAILURE" || action.Status != "ACTION_REQUIRED" || len(action.Failures) != 1 || action.Failures[0].JobID != "job" || action.Failures[0].AttemptID != "attempt" || action.Failures[0].ReportedResult != "CHECK_FAILED" {
						t.Fatalf("HTTP failed-build action: %s %v", httpReply["agent_action"], err)
					}
					session, err := mcp.NewClient(&mcp.Implementation{Name: "build-investigation-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer session.Close()
					reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_operation", Arguments: map[string]any{"operation_id": "parent"}})
					if err != nil || reply.IsError {
						t.Fatal("MCP", err, reply)
					}
					b, _ := json.Marshal(reply.StructuredContent)
					var mcpReply map[string]json.RawMessage
					if err := json.Unmarshal(b, &mcpReply); err != nil {
						t.Fatal(err)
					}
					var left, right any
					_ = json.Unmarshal(httpReply["agent_action"], &left)
					_ = json.Unmarshal(mcpReply["agent_action"], &right)
					lhs, _ := json.Marshal(left)
					rhs, _ := json.Marshal(right)
					if string(lhs) != string(rhs) || phase == 1 && string(lhs) != originalAction {
						t.Fatal("HTTP/MCP/reopen action mismatch")
					}
					originalAction = string(lhs)
					if len(lhs) > 32<<10 || strings.Contains(string(lhs), "PRIVATE") {
						t.Fatal("new bounded projection leaked private fields")
					}
					after, _ := store.Read(ctx)
					afterJSON, _ := json.Marshal(after)
					if string(beforeJSON) != string(afterJSON) {
						t.Fatal("HTTP/MCP reads mutated persisted state")
					}
				}()
			}
		})
	}
}
