package application

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	rr "releasecontrol/internal/runtimereport"
)

type privateRead func(context.Context, *Service, string) (any, error)

var privateProductReadMatrix = map[string]privateRead{
	"ListCodeRepositories": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.ListCodeRepositories(ctx, product, "")
	},
	"GetCodeMap": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.GetCodeMap(ctx, domain.CodeMapRequest{ProductID: product, RepositoryID: product + "-repository"})
	},
	"SearchCode": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.SearchCode(ctx, domain.CodeSearchRequest{ProductID: product, Query: "fixturekeyword"})
	},
	"GetCodeFileOutline": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.GetCodeFileOutline(ctx, domain.CodeOutlineRequest{ProductID: product, RepositoryID: product + "-repository", Path: "README.md"})
	},
	"GetCodeIndexStatus": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.GetCodeIndexStatus(ctx, domain.CodeStatusRequest{ProductID: product})
	},
	"ProjectContext": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.ProjectContext(ctx, product)
	},
	"ProjectContextSummary": func(ctx context.Context, service *Service, product string) (any, error) {
		result, err := service.ProjectContextSummary(ctx, product)
		if err == nil && product == privateFixture {
			card := result.(map[string]any)["product"].(map[string]any)
			members := card["members"].([]string)
			if card["visibility"] != "private" || card["owner"] != "A" || len(members) != 1 || members[0] != "B" {
				return nil, fmt.Errorf("unexpected visible membership: %v", card)
			}
		}
		return result, err
	},
	"Resume": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.Resume(ctx, product+"-feature")
	},
	"ResumeSummary": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.ResumeSummary(ctx, product+"-feature")
	},
	"StatePageProduct": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.StatePage(ctx, domain.StatePageRequest{ProductID: product})
	},
	"StatePageFeature": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.StatePage(ctx, domain.StatePageRequest{FeatureID: product + "-feature"})
	},
	"UIStateProduct": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.UIState(ctx, product, "")
	},
	"UIStateFeature": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.UIState(ctx, "", product+"-feature")
	},
	"SearchContext": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.SearchContext(ctx, domain.ContextSearchRequest{ProductID: product, Query: "fixturekeyword", IncludeCode: true})
	},
	"ContextExcerpt": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.ContextExcerpt(ctx, domain.ContextExcerptRequest{ProductID: product, ID: product + "-feature"})
	},
	"GraphNeighbors": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.GraphNeighbors(ctx, domain.GraphNeighborsRequest{ProductID: product, ID: product + "-feature"})
	},
	"SearchKnowledge": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.SearchKnowledge(ctx, product, "fixturekeyword", 5)
	},
	"ExportWorkspace": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.ExportWorkspace(ctx, product)
	},
	"LocalGitState": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.LocalGitState(ctx, product, product+"-checkout")
	},
	"ListBuildProfiles": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.ListBuildProfiles(ctx, product)
	},
	"ListBuilds": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.ListBuilds(ctx, domain.ListBuildsRequest{ProductID: product})
	},
	"GetBuild": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.GetBuild(ctx, domain.BuildLookup{ProductID: product, BuildID: product + "-build"})
	},
	"ListDeployments": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.ListDeployments(ctx, domain.ListDeploymentsRequest{ProductID: product})
	},
	"GetDeployment": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.GetDeployment(ctx, domain.DeploymentLookup{ProductID: product, DeploymentID: product + "-deployment"})
	},
	"ListPipelines": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.ListPipelines(ctx, domain.ListPipelinesRequest{ProductID: product})
	},
	"ListSignatures": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.ListSignatures(ctx, domain.ListSignaturesRequest{ProductID: product})
	},
	"ListProdPromotions": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.ListProdPromotions(ctx, product, 5)
	},
	"GetProdPromotion": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.GetProdPromotion(ctx, product, product+"-promotion")
	},
	"WorkerHistoryJobs": func(ctx context.Context, service *Service, product string) (any, error) {
		return privateWorkerHistory(ctx, service, domain.WorkerHistoryRequest{ProductID: product, FeatureID: product + "-feature"}, product+"-job")
	},
	"WorkerHistoryAttempts": func(ctx context.Context, service *Service, product string) (any, error) {
		return privateWorkerHistory(ctx, service, domain.WorkerHistoryRequest{ProductID: product, JobID: product + "-job", Collection: "attempts"}, product+"-attempt")
	},
	"WorkerHistoryLinks": func(ctx context.Context, service *Service, product string) (any, error) {
		return privateWorkerHistory(ctx, service, domain.WorkerHistoryRequest{ProductID: product, JobID: product + "-job", Collection: "links"}, product+"-revision")
	},
	"GetRuntimeReport": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.GetRuntimeReport(ctx, product, product+"-environment", "")
	},
	"ReadReferences": func(ctx context.Context, service *Service, product string) (any, error) {
		ids := []string{product + "-integration", product + "-binding", product + "-event", product + "-audit", product + "-environment-binding"}
		if err := service.authorizeReferences(ctx, "read", ids...); err != nil {
			return nil, err
		}
		var projected domain.State
		if reader, ok := service.store.(referenceReader); ok {
			state, err := reader.ReadReferences(ctx, ids)
			if err != nil {
				return nil, err
			}
			projected = state
		} else if err := service.view(ctx, func(raw *domain.State) error {
			visible, err := scopedState(ctx, *raw)
			if err != nil {
				return err
			}
			projected, _ = projectReferences(visible, ids)
			return nil
		}); err != nil {
			return nil, err
		}
		if len(projected.Integrations) != 1 || len(projected.RepositoryBindings) != 1 || len(projected.ExecutionEvents) != 1 || len(projected.Events) != 1 || len(projected.Environments) != 1 {
			return nil, fmt.Errorf("unexpected reference projection for %s", product)
		}
		return projected, nil
	},
	"SyncReviewPreview": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.SyncReview(ctx, "fixture", product+"-integration", product+"-repository", 1, nil, true)
	},
	"get_integration_context": privateQuery("get_integration_context", "-integration"),
	"get_integration_git":     privateQuery("get_integration_git", "-integration"),
	"get_execution_group":     privateQuery("get_execution_group", "-group"),
	"list_executions":         privateQuery("list_executions", ""),
	"list_execution_events":   privateQuery("list_execution_events", ""),
	"list_attention":          privateQuery("list_attention", ""),
	"get_question":            privateQuery("get_question", "-question"),
	"list_questions": func(ctx context.Context, service *Service, product string) (any, error) {
		return service.Query(ctx, "list_questions", fmt.Sprintf(`{"product_id":%q,"addressed_to":"A"}`, product))
	},
	"get_dependency_graph":      privateQuery("get_dependency_graph", ""),
	"next_work":                 privateQuery("next_work", ""),
	"status_overview":           privateQuery("status_overview", ""),
	"get_feature_graph":         privateQuery("get_feature_graph", "-feature"),
	"get_operation":             privateQuery("get_operation", "-operation"),
	"get_intelligence_task":     privateQuery("get_intelligence_task", "-intelligence"),
	"get_environment_state":     privateQuery("get_environment_state", "-environment"),
	"get_worker_job":            privateQuery("get_worker_job", "-job"),
	"get_artifact_retention":    privateQuery("get_artifact_retention", ""),
	"get_product_configuration": privateQuery("get_product_configuration", ""),
	"get_unlinked_git_commits":  privateQuery("get_unlinked_git_commits", ""),
	"list_branches":             privateQuery("list_branches", "-repository"),
}

func privateWorkerHistory(ctx context.Context, service *Service, request domain.WorkerHistoryRequest, expectedID string) (any, error) {
	page, err := service.WorkerHistory(ctx, request)
	if err == nil && (len(page.Items) != 1 || page.Items[0].ID != expectedID) {
		return nil, fmt.Errorf("expected worker history item %q, got %+v", expectedID, page.Items)
	}
	return page, err
}

func privateQuery(name, suffix string) privateRead {
	return func(ctx context.Context, service *Service, product string) (any, error) {
		return service.Query(ctx, name, product+suffix)
	}
}

const privateFixture = "matrix-private"
const sharedFixture = "matrix-shared"

type privateFixtureStore interface {
	Store
	rr.Store
}

type privateBoundedFixture interface {
	stateMetadataReader
	uiStateReader
	contextRetrievalReader
	workerHistoryReader
	RuntimeFreshnessStore
	rr.Store
}

// The application memory fake has no bounded reader implementation. The immutable
// fixture is mirrored to a SQLite adapter only for those interfaces, not for State
// or reference authorization (which retain their real in-memory code paths).
type privateMemoryFixture struct {
	*memoryStore
	privateBoundedFixture
}

func privatePostgresFixture(t *testing.T) *persistence.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not configured")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || !strings.HasSuffix(parsed.Path, "_tests") {
		t.Fatal("TEST_DATABASE_URL must be a dedicated *_tests database URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	schema := pgx.Identifier{fmt.Sprintf("private_matrix_%d", time.Now().UnixNano())}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DROP SCHEMA "+schema.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	query := parsed.Query()
	query.Set("search_path", schema[0])
	parsed.RawQuery = query.Encode()
	store, err := persistence.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}

func privateLocalFixture(t *testing.T) *persistence.SQLiteStore {
	t.Helper()
	store, err := persistence.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "privacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}

func privateDigest(product string) string {
	if product == privateFixture {
		return "sha256:" + strings.Repeat("a", 64)
	}
	return "sha256:" + strings.Repeat("b", 64)
}

func seedPrivateReadMatrix(t *testing.T, store privateFixtureStore, workspace string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	err := store.Update(ctx, func(state *domain.State) error {
		state.GitHubConnections = append(state.GitHubConnections, domain.GitHubConnection{Meta: domain.Meta{ID: "matrix-connection"}})
		for _, product := range []string{privateFixture, sharedFixture} {
			meta := func(suffix string) domain.Meta {
				return domain.Meta{ID: product + suffix, ProductID: product, CreatedAt: now, UpdatedAt: now}
			}
			featureMeta := func(suffix string) domain.Meta {
				out := meta(suffix)
				out.FeatureID = product + "-feature"
				return out
			}
			entry := domain.Product{Meta: meta(""), Name: product + " title", Description: product + " text", Visibility: "shared"}
			entry.ProductID = ""
			if product == privateFixture {
				entry.Visibility, entry.Owner, entry.Members = "private", "A", []string{"B"}
			}
			state.Products = append(state.Products, entry)
			state.Features = append(state.Features, domain.Feature{Meta: meta("-feature"), Title: product + " feature title", Goal: product + " fixturekeyword text", Status: "planned"})
			state.Memories = append(state.Memories, domain.Memory{Meta: featureMeta("-question"), Kind: "question", Status: "open", Title: product + " owner question", Question: &domain.QuestionDetails{Kind: "question", AddressedTo: "A", AskedBy: "fixture"}})
			state.Integrations = append(state.Integrations, domain.Integration{Meta: featureMeta("-integration"), Title: product + " integration title", Objective: product + " fixturekeyword integration", Status: "planned"})
			state.Repositories = append(state.Repositories, domain.Repository{Meta: meta("-repository"), Name: product + " repository", URL: "https://example.invalid/fixture/" + product, Role: "APPLICATION"})
			state.RepositoryBindings = append(state.RepositoryBindings, domain.RepositoryBinding{Meta: meta("-binding"), RepositoryID: product + "-repository", ConnectionID: "matrix-connection", FullName: "fixture/" + product, Role: "APPLICATION"})
			state.ConnectionGrants = append(state.ConnectionGrants, domain.ConnectionGrant{Meta: meta("-grant"), ConnectionID: "matrix-connection"})
			state.LocalCheckouts = append(state.LocalCheckouts, domain.LocalCheckout{Meta: meta("-checkout"), RepositoryID: product + "-repository", Workspace: workspace, RelativePath: product})
			state.Environments = append(state.Environments, domain.Environment{Meta: meta("-environment"), Name: product + " environment"})
			state.RuntimeObservations = append(state.RuntimeObservations, domain.RuntimeObservation{Meta: meta("-observation"), Target: "fixture", OperationID: product + "-operation", EnvironmentID: product + "-environment", Healthy: true})
			state.EnvironmentBindings = append(state.EnvironmentBindings, domain.EnvironmentBinding{Meta: meta("-environment-binding"), EnvironmentID: product + "-environment"})
			state.Operations = append(state.Operations,
				domain.ExternalOperation{Meta: featureMeta("-operation"), Kind: "REFRESH_GIT", Status: "FAILED", Detail: product + " operation text"},
				domain.ExternalOperation{Meta: featureMeta("-intelligence"), Kind: "INTELLIGENCE", Status: "FAILED", Intelligence: &domain.IntelligenceTask{}})
			state.Builds = append(state.Builds, domain.Build{Meta: meta("-build"), ProfileID: product + "-profile", Status: "SUCCEEDED"})
			state.Deployments = append(state.Deployments, domain.Deployment{Meta: meta("-deployment"), BuildID: product + "-build", EnvironmentID: "dev", Status: "SUCCEEDED"})
			state.ArtifactSignatures = append(state.ArtifactSignatures, domain.ArtifactSignature{Meta: meta("-signature"), Status: "SIGNED"})
			state.ProdPromotions = append(state.ProdPromotions, domain.ProdPromotion{Meta: meta("-promotion"), Status: "SUCCEEDED", Items: []domain.ProdPromotionItem{{
				Digest: privateDigest(product), Result: "SIGNED", ProdImageRef: "fixture/" + product, ProdSignature: &domain.ProdSignature{SignatureTag: product + "-tag"},
			}}})
			members := []string{product + "-integration"}
			state.ExecutionGroups = append(state.ExecutionGroups, domain.ExecutionGroup{ID: product + "-group", ProductID: product, Name: product + " group", Owner: "A", MemberIntegrationIDs: members, Revisions: []domain.ExecutionGroupRevision{{Revision: 1, MemberIntegrationIDs: members}}})
			state.IntegrationExecutions = append(state.IntegrationExecutions, domain.IntegrationExecution{ID: product + "-execution", ProductID: product, IntegrationID: product + "-integration", State: "unassigned", DesiredState: "ready"})
			state.ExecutionEvents = append(state.ExecutionEvents, domain.ExecutionEvent{ID: product + "-event", ProductID: product, IntegrationID: product + "-integration", CreatedAt: now, EscalationRequired: true})
			state.Events = append(state.Events, domain.Event{ID: product + "-audit", ProductID: product, At: now})
			sha := strings.Repeat("a", 40)
			state.IntegrationRevisions = append(state.IntegrationRevisions, domain.IntegrationRevision{Meta: featureMeta("-revision"), IntegrationID: product + "-integration", RepositoryID: product + "-repository", HeadCommit: sha})
			state.WorkerJobs = append(state.WorkerJobs, domain.WorkerJob{Meta: featureMeta("-job"), OperationID: product + "-operation", IntegrationID: product + "-integration", Status: "SUCCEEDED", RequestKey: product + "-job", Inputs: domain.WorkerInputs{RepositoryID: product + "-repository", RevisionID: product + "-revision", SourceSHA: sha}})
			state.WorkerAttempts = append(state.WorkerAttempts, domain.WorkerAttempt{Meta: featureMeta("-attempt"), JobID: product + "-job", Fence: 1, Status: "SUCCEEDED"})
			state.GitObservations = append(state.GitObservations, domain.GitObservation{Meta: meta("-git"), RepositoryID: product + "-repository", Branch: "main", Status: "unknown", Commits: []delivery.CommitInfo{{SHA: strings.Repeat("c", 40), Message: product + " unlinked"}}})
			state.ProjectKnowledge = append(state.ProjectKnowledge, domain.ProjectKnowledge{Meta: meta("-knowledge"), Overview: product + " overview", Nodes: []domain.KnowledgeNode{{Meta: meta("-node"), Kind: "contract", Title: product + " fixturekeyword", Content: product + " knowledge", Status: "active"}}})
		}
		state.Products = append(state.Products, domain.Product{Meta: domain.Meta{ID: "matrix-z-shared"}, Name: "Last shared"})
		state.FeatureDependencies = append(state.FeatureDependencies, domain.FeatureDependency{Meta: domain.Meta{ID: "matrix-cross-edge", ProductID: sharedFixture, FeatureID: sharedFixture + "-feature", CreatedAt: now, UpdatedAt: now}, FromFeatureID: sharedFixture + "-feature", ToFeatureID: privateFixture + "-feature", Type: "requires", Status: "accepted"})
		return nil
	})
	if err != nil {
		t.Fatal("seed state:", err)
	}
	for _, product := range []string{privateFixture, sharedFixture} {
		if _, mirrored := store.(*privateMemoryFixture); mirrored {
			break
		}
		if _, err := store.SaveRuntimeReport(ctx, rr.Report{ProductID: product, EnvironmentID: product + "-environment", Cluster: product + "-cluster", ReportedAt: now}, now); err != nil {
			t.Fatal("seed runtime report:", err)
		}
		if err := store.(RuntimeFreshnessStore).TouchRuntimeFreshness(ctx, product+"-operation", "fixture", product+"-observation", now, true); err != nil {
			t.Fatal("seed runtime freshness:", err)
		}
	}
}

func privateResult(t *testing.T, name, product string, result any, err error, visible bool) {
	t.Helper()
	if visible && err != nil || !visible && !errors.Is(err, ErrNotFound) {
		t.Errorf("%s (%s): visible=%t, error=%v", name, product, visible, err)
		return
	}
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if !visible && strings.Contains(string(raw), privateFixture) {
		t.Errorf("%s returned private data with an error: %s", name, raw)
	}
	if visible && !strings.Contains(string(raw), product) {
		t.Errorf("%s omitted visible fixture %s: %s", name, product, raw)
	}
}

func TestPrivateProductReadMatrix(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, product := range []string{privateFixture, sharedFixture} {
		path := filepath.Join(workspace, product)
		runGit := func(input string, args ...string) string {
			command := osexec.Command("git", args...)
			command.Stdin = strings.NewReader(input)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("fixture git %v: %v: %s", args, err, output)
			}
			return strings.TrimSpace(string(output))
		}
		runGit("", "init", "--quiet", "--initial-branch=main", path)
		tree := runGit("", "-C", path, "mktree")
		head := runGit("fixture\n", "-C", path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", tree)
		runGit("", "-C", path, "update-ref", "refs/heads/main", head)
		runGit("", "-C", path, "remote", "add", "origin", "https://example.invalid/fixture/"+product)
	}
	for _, backend := range []string{"memory", "sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store privateFixtureStore
			if backend == "postgres" {
				store = privatePostgresFixture(t)
			} else {
				local := privateLocalFixture(t)
				store = local
				if backend == "memory" {
					seedPrivateReadMatrix(t, local, workspace)
					store = &privateMemoryFixture{&memoryStore{state: domain.EmptyState()}, local}
				}
			}
			seedPrivateReadMatrix(t, store, workspace)
			service := New(store)
			for _, product := range []string{privateFixture, sharedFixture} {
				repository := product + "-repository"
				doc := domain.CodeIndex{RepositoryID: repository, Branch: "main", Commit: strings.Repeat("a", 40), Version: domain.CodeIndexVersion, IndexedAt: time.Now(), Files: []domain.CodeFile{{Path: "README.md", Language: "Markdown", Size: 128, Lines: 1, Doc: product + " fixturekeyword"}}}
				if _, err := service.UpsertCodeIndex(context.Background(), domain.CodeIndexWrite{ProductID: product, RepositoryID: repository, Document: doc}); err != nil {
					t.Fatal("seed code index:", err)
				}
			}
			service.SetWorkspaceRoot(workspace)
			service.provider = &privateBranchFixture{}
			service.prodPublicKey = "fixture-public-key"
			service.builds = &buildRuntime{config: &buildconfig.Config{Profiles: []buildconfig.Profile{
				{ID: privateFixture + "-profile", ProductID: privateFixture}, {ID: sharedFixture + "-profile", ProductID: sharedFixture},
			}}}
			runPrivateReadMatrix(t, service)
		})
	}
}

type privateBranchFixture struct{ fakeDelivery }

func (privateBranchFixture) Branches(_ context.Context, _ delivery.Connection, repository string) ([]delivery.BranchInfo, error) {
	return []delivery.BranchInfo{{Name: "branch/" + repository}}, nil
}

func (privateBranchFixture) PullRequestReview(_ context.Context, _ delivery.Connection, repository string, number int) (delivery.PullRequestReview, error) {
	return delivery.PullRequestReview{Number: number, Head: repository}, nil
}

func runPrivateReadMatrix(t *testing.T, service *Service) {
	t.Helper()
	callers := []struct {
		name    string
		ctx     context.Context
		visible bool
	}{
		{"owner", userCtx("A", "human", "", "member"), true}, {"agent owner", userCtx("a1", "agent", "A", "member"), true},
		{"member", userCtx("B", "human", "", "member"), true},
		{"outsider", userCtx("C", "human", "", "member"), false},
		{"agent outsider", userCtx("c1", "agent", "C", "member"), false},
		{"admin outsider", userCtx("D", "human", "", "admin"), false},
		{"bootstrap", userCtx("service/bootstrap", "service", "", "admin"), true},
		{"local", context.Background(), true},
	}
	for _, tc := range callers {
		t.Run(tc.name, func(t *testing.T) {
			for name, read := range privateProductReadMatrix {
				t.Run(name, func(t *testing.T) {
					var denied error
					for _, product := range []string{sharedFixture, privateFixture} {
						result, err := read(tc.ctx, service, product)
						visible := product != privateFixture || tc.visible
						if visible && name == "get_execution_group" && tc.name != "local" && workUser(tc.ctx) != "A" {
							if result != nil || !errors.Is(err, auth.ErrForbidden) || !strings.Contains(err.Error(), "NOT_ASSIGNEE") {
								t.Errorf("%s (%s): expected owned-group denial, got %v, %v", name, product, result, err)
							}
							continue
						}
						privateResult(t, name, product, result, err, visible)
						if !visible {
							denied = err
						}
						if !tc.visible && product == sharedFixture {
							raw, _ := json.Marshal(result)
							if strings.Contains(string(raw), privateFixture) {
								t.Errorf("%s returned private data from a shared scope: %s", name, raw)
							}
						}
					}
					if !tc.visible {
						if _, err := read(tc.ctx, service, "matrix-missing"); !errors.Is(err, ErrNotFound) {
							t.Errorf("missing scope should also be NOT_FOUND: %v", err)
						} else if denied != nil && strings.ReplaceAll(denied.Error(), privateFixture, "matrix-missing") != err.Error() {
							t.Errorf("hidden and missing differ: %v / %v", denied, err)
						}
					}
				})
			}
			checkPrivateAggregateReads(t, service, tc.ctx, tc.visible, tc.name)
		})
	}
}

func checkPrivateAggregateReads(t *testing.T, service *Service, ctx context.Context, visible bool, caller string) {
	t.Helper()
	checks := map[string]func() (any, error){
		"State":   func() (any, error) { return service.State(ctx) },
		"UIState": func() (any, error) { return service.UIState(ctx, "", "") },
		"RuntimeFreshness": func() (any, error) {
			return service.store.(RuntimeFreshnessStore).RuntimeFreshness(ctx, []string{
				domain.RuntimeObservationKey(privateFixture+"-operation", "fixture"),
				domain.RuntimeObservationKey(sharedFixture+"-operation", "fixture"),
			})
		},
		"StatePage": func() (any, error) {
			var ids []string
			var cursor string
			for len(ids) < 4 {
				page, err := service.StatePage(ctx, domain.StatePageRequest{Limit: 1, Cursor: cursor})
				if err != nil {
					return nil, err
				}
				if len(page.Items) != 1 || page.Pagination.Returned != 1 {
					t.Fatalf("hidden product shifted navigation counts: %+v", page)
				}
				ids = append(ids, page.Items[0].ID)
				if page.Items[0].ID == privateFixture {
					card := page.Items[0]
					if card.Visibility != "private" || card.Owner != "A" || len(card.Members) != 1 || card.Members[0] != "B" {
						t.Fatalf("missing visible membership: %+v", card)
					}
				}
				cursor = page.Pagination.NextCursor
				if !page.Pagination.HasMore {
					break
				}
			}
			if expected := 2 + map[bool]int{true: 1}[visible]; len(ids) != expected || !visible && ids[0] != sharedFixture || ids[len(ids)-1] != "matrix-z-shared" {
				t.Fatalf("navigation pages: got %v, want %d visible rows", ids, expected)
			}
			if !visible {
				for _, anchor := range []string{privateFixture, "matrix-missing"} {
					cursor := encodeStateCursor(domain.StateMetadataRequest{Collection: "products"}, anchor)
					if _, err := service.StatePage(ctx, domain.StatePageRequest{Limit: 1, Cursor: cursor}); !errors.Is(err, ErrNotFound) {
						t.Errorf("invisible navigation anchor %q: %v", anchor, err)
					}
				}
			}
			return ids, nil
		},
	}
	for name, read := range checks {
		t.Run(name, func(t *testing.T) {
			result, err := read()
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(result)
			if err != nil || !strings.Contains(string(raw), sharedFixture) || strings.Contains(string(raw), privateFixture) != visible {
				t.Errorf("visible=%t: %v %s", visible, err, raw)
			}
		})
	}
	t.Run("PublicProdSignatures", func(t *testing.T) {
		shared, err := service.PublicProdSignatures(ctx, privateDigest(sharedFixture))
		privateResult(t, "public shared signature", sharedFixture, shared, err, true)
		private, err := service.PublicProdSignatures(ctx, privateDigest(privateFixture))
		privateResult(t, "public private signature", privateFixture, private, err, false)
	})
	t.Run("CreateBackup", func(t *testing.T) {
		backup, err := service.CreateBackup(ctx, "fixture")
		if caller == "bootstrap" || caller == "local" {
			if err != nil {
				t.Fatal(err)
			}
			archive, err := base64.StdEncoding.DecodeString(backup.ArchiveBase64)
			if err != nil || len(archive) == 0 {
				t.Fatalf("missing backup archive: %v", err)
			}
			reader, err := gzip.NewReader(bytes.NewReader(archive))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			var envelope backupEnvelope
			if err := json.NewDecoder(reader).Decode(&envelope); err != nil || !productExists(&envelope.State, privateFixture) || !productExists(&envelope.State, sharedFixture) {
				t.Fatalf("trusted backup must contain both Products: %v", err)
			}
		} else if !errors.Is(err, auth.ErrForbidden) || backup.ArchiveBase64 != "" {
			t.Fatalf("unexpected backup authorization: %v", err)
		}
	})
}

func TestPrivateProductWriteMatrix(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store Store
			switch backend {
			case "memory":
				store = &memoryStore{state: domain.EmptyState()}
			case "sqlite":
				store = privateLocalFixture(t)
			case "postgres":
				store = privatePostgresFixture(t)
			}
			runPrivateWriteMatrix(t, store)
		})
	}
}

func runPrivateWriteMatrix(t *testing.T, store Store) {
	t.Helper()
	service := New(store)
	owner := userCtx("A", "human", "", "member")
	for _, c := range []domain.Command{{Action: "create_product", ID: "P", Data: map[string]any{"name": "Private", "visibility": "private"}}, {Action: "create_feature", ID: "PF", ProductID: "P", Data: map[string]any{"title": "Secret", "goal": "secret"}}, {Action: "create_integration", ID: "PI", FeatureID: "PF", Data: map[string]any{"title": "Secret", "objective": "secret"}}} {
		if _, err := service.Execute(owner, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		caller context.Context
	}{{"C", userCtx("C", "human", "", "member")}, {"c1", userCtx("c1", "agent", "C", "member")}, {"D", userCtx("D", "human", "", "admin")}} {
		for _, c := range []domain.Command{{Action: "create_feature", ProductID: "P", Data: map[string]any{"title": "probe", "goal": "probe"}}, {Action: "create_integration", FeatureID: "PF", Data: map[string]any{"title": "probe", "objective": "probe"}}, {Action: "record_progress", IntegrationID: "PI", Data: map[string]any{"completed": []string{"probe"}}}, {Action: "claim_execution", IntegrationID: "PI", Data: map[string]any{}}} {
			if _, err := service.Execute(tc.caller, c); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s: %s returned %v, want NOT_FOUND", tc.name, c.Action, err)
			}
		}
	}
}
