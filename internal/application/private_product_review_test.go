package application

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	rr "releasecontrol/internal/runtimereport"
	"releasecontrol/internal/workerconfig"
)

func privateReviewStores(t *testing.T, run func(*testing.T, Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { run(t, &memoryStore{domain.EmptyState()}) })
	t.Run("sqlite", func(t *testing.T) { run(t, privateLocalFixture(t)) })
	t.Run("postgres", func(t *testing.T) { run(t, privatePostgresFixture(t)) })
}

func privateReviewCreate(t *testing.T, service *Service, ctx context.Context, key, visibility string) domain.Product {
	t.Helper()
	value, err := service.Execute(ctx, domain.Command{Action: "create_product", ID: key, Actor: "test", Data: map[string]any{"name": "name-" + key, "description": "description-" + key, "visibility": visibility}})
	if err != nil {
		t.Fatal(err)
	}
	return value.(domain.Product)
}

func TestPrivateDirectMutationSurface(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		service := New(store)
		owner := userCtx("owner", "human", "", "admin")
		privateReviewCreate(t, service, owner, "secret", "private")
		meta := func(key string) domain.Meta { return domain.Meta{ID: key, ProductID: "secret"} }
		if err := store.Update(context.Background(), func(st *domain.State) error {
			st.Features = append(st.Features, domain.Feature{Meta: meta("feature"), Status: "active"})
			st.Integrations = append(st.Integrations, domain.Integration{Meta: domain.Meta{ID: "integration", ProductID: "secret", FeatureID: "feature"}, Status: "planned"})
			st.Repositories = append(st.Repositories, domain.Repository{Meta: meta("repo"), Role: "APPLICATION"})
			st.Builds = append(st.Builds, domain.Build{Meta: meta("build"), Status: "QUEUED"})
			st.WorkerJobs = append(st.WorkerJobs, domain.WorkerJob{Meta: meta("job"), Status: "QUEUED", Capability: "registry_publish"})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		calls := map[string]func(context.Context) error{
			"UpsertCodeIndex": func(ctx context.Context) error {
				_, err := service.UpsertCodeIndex(ctx, domain.CodeIndexWrite{ProductID: "secret", RepositoryID: "repo"})
				return err
			},
			"Execute": func(ctx context.Context) error {
				_, err := service.Execute(ctx, domain.Command{Action: "record_progress", IntegrationID: "integration"})
				return err
			},
			"QueueBuild": func(ctx context.Context) error {
				_, err := service.QueueBuild(ctx, domain.QueueBuildRequest{ProductID: "secret"})
				return err
			},
			"CancelBuild": func(ctx context.Context) error {
				_, err := service.CancelBuild(ctx, domain.BuildLookup{ProductID: "secret", BuildID: "build"})
				return err
			},
			"CloseBuild": func(ctx context.Context) error {
				_, err := service.CloseBuild(ctx, domain.CloseBuildRequest{ProductID: "secret", BuildID: "build"})
				return err
			},
			"DeployBuild": func(ctx context.Context) error {
				_, err := service.DeployBuild(ctx, domain.DeployBuildRequest{ProductID: "secret", BuildID: "build", EnvironmentID: "dev"})
				return err
			},
			"CloseDeployment": func(ctx context.Context) error {
				_, err := service.CloseDeployment(ctx, domain.CloseDeploymentRequest{ProductID: "secret", DeploymentID: "deploy"})
				return err
			},
			"ReconcileDeployment": func(ctx context.Context) error {
				_, err := service.ReconcileDeployment(ctx, domain.ReconcileDeploymentRequest{ProductID: "secret", DeploymentID: "deploy", Reason: "test recovery"})
				return err
			},
			"SignArtifact": func(ctx context.Context) error {
				_, err := service.SignArtifact(ctx, domain.SignArtifactRequest{ProductID: "secret"})
				return err
			},
			"RequestProdPromotion": func(ctx context.Context) error {
				_, err := service.RequestProdPromotion(ctx, domain.RequestProdPromotion{ProductID: "secret"})
				return err
			},
			"ClaimProdPromotion": func(ctx context.Context) error {
				_, err := service.ClaimProdPromotion(ctx, domain.ClaimProdPromotion{ProductID: "secret"})
				return err
			},
			"RecordProdPromotionResult": func(ctx context.Context) error {
				_, err := service.RecordProdPromotionResult(ctx, "promotion", "secret", "prod", domain.ProdPromotionResult{})
				return err
			},
			"HeartbeatProdPromotion": func(ctx context.Context) error {
				_, err := service.HeartbeatProdPromotion(ctx, "promotion", "secret", "prod", domain.HeartbeatProdPromotion{})
				return err
			},
			"ClaimWorker": func(ctx context.Context) error {
				_, err := service.ClaimWorker(ctx, workerconfig.Principal{}, domain.WorkerClaim{ProductID: "secret"})
				return err
			},
			"StartWorker": func(ctx context.Context) error {
				_, err := service.StartWorker(ctx, workerconfig.Principal{}, domain.WorkerAttemptRequest{JobID: "job"})
				return err
			},
			"HeartbeatWorker": func(ctx context.Context) error {
				_, err := service.HeartbeatWorker(ctx, workerconfig.Principal{}, domain.WorkerAttemptRequest{JobID: "job"})
				return err
			},
			"ReportWorker": func(ctx context.Context) error {
				_, err := service.ReportWorker(ctx, workerconfig.Principal{}, domain.WorkerReport{WorkerAttemptRequest: domain.WorkerAttemptRequest{JobID: "job"}})
				return err
			},
			"ReportWorkerPublication": func(ctx context.Context) error {
				_, err := service.ReportWorker(ctx, workerconfig.Principal{}, domain.WorkerReport{WorkerAttemptRequest: domain.WorkerAttemptRequest{JobID: "job"}, Result: "PASSED"})
				return err
			},
			"ScanWorkspace": func(ctx context.Context) error {
				_, err := service.ScanWorkspace(ctx, "secret", "test")
				return err
			},
			"MatchWorkspace": func(ctx context.Context) error {
				_, err := service.MatchWorkspace(ctx, "secret", "test")
				return err
			},
			"SetProjectKnowledge": func(ctx context.Context) error {
				_, err := service.SetProjectKnowledge(ctx, "secret", "test", domain.ProjectKnowledge{})
				return err
			},
			"UpsertKnowledgeNode": func(ctx context.Context) error {
				_, err := service.UpsertKnowledgeNode(ctx, "secret", "test", domain.KnowledgeNode{})
				return err
			},
			"PlanLocalGitSync": func(ctx context.Context) error {
				_, err := service.PlanLocalGitSync(ctx, LocalGitRequest{ProductID: "secret"})
				return err
			},
			"RecordLocalGitSync": func(ctx context.Context) error {
				_, err := service.RecordLocalGitSync(ctx, LocalGitRequest{ProductID: "secret"})
				return err
			},
			"SyncReview": func(ctx context.Context) error {
				_, err := service.SyncReview(ctx, "test", "integration", "repo", 1, nil, false)
				return err
			},
			"ReportRuntime": func(ctx context.Context) error {
				_, err := service.ReportRuntime(ctx, rr.Report{ProductID: "secret"})
				return err
			},
		}
		outsiders := []context.Context{userCtx("outsider", "human", "", "admin"), userCtx("agent", "agent", "outsider", "member"), auth.WithPrincipal(context.Background(), auth.Principal{ID: "reader", Kind: "human", Products: []string{"other"}, Permissions: []string{"read"}})}
		for name, call := range calls {
			t.Run(name, func(t *testing.T) {
				for _, outsider := range outsiders {
					if err := call(outsider); !errors.Is(err, ErrNotFound) {
						t.Errorf("expected NOT_FOUND, got %v", err)
					}
				}
			})
		}
		if build, err := service.CancelBuild(owner, domain.BuildLookup{ProductID: "secret", BuildID: "build"}); err != nil || build.Status != "CANCELLED" {
			t.Fatalf("owner could not cancel original queued build: %+v %v", build, err)
		}
		if durable, ok := store.(rr.Store); ok {
			if _, err := durable.SaveRuntimeReport(outsiders[0], rr.Report{ProductID: "secret"}, time.Now()); !errors.Is(err, domain.ErrReadScopeNotFound) {
				t.Fatalf("transaction did not reject invisible Product: %v", err)
			}
		}
	})
}

func TestPrivateProductOwnershipGrantsAndImport(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		service := New(store)
		owner := userCtx("owner", "human", "", "admin")
		outsider := userCtx("outsider", "human", "", "admin")
		if got := privateReviewCreate(t, service, owner, "shared-human", "shared"); got.Owner != "owner" {
			t.Fatal(got)
		}
		limited := auth.WithPrincipal(context.Background(), auth.Principal{ID: "ag", Kind: "agent", Owner: "owner", Products: []string{"allowed"}, Permissions: []string{"read", "write"}})
		for _, key := range []string{"", "outside"} {
			if _, err := service.Execute(limited, domain.Command{Action: "create_product", ID: key, Data: map[string]any{"name": "denied"}}); !errors.Is(err, auth.ErrForbidden) {
				t.Fatalf("limited credential created %q: %v", key, err)
			}
		}
		if got := privateReviewCreate(t, service, limited, "allowed", "shared"); got.Owner != "owner" {
			t.Fatal(got)
		}
		claim := domain.Command{Action: "set_product_visibility", ProductID: "shared-human", Data: map[string]any{"visibility": "private"}}
		if _, err := service.Execute(outsider, claim); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("owned shared product was claimable", err)
		}
		privateReviewCreate(t, service, context.Background(), "ownerless", "shared")
		claim.ProductID = "ownerless"
		if _, err := service.Execute(outsider, domain.Command{Action: "adopt_product_ownership", ProductID: "ownerless"}); err != nil {
			t.Fatal(err)
		}
		if got, err := service.Execute(outsider, claim); err != nil || got.(domain.Product).Owner != "outsider" {
			t.Fatal(got, err)
		}
		in := WorkspaceConfiguration{Format: "release-control-workspace", Product: domain.Product{Meta: domain.Meta{ID: "import"}, Name: "Imported", Visibility: "private", Owner: "foreign", Members: []string{"outsider", "owner"}}}
		if _, err := service.ImportWorkspace(context.Background(), "test", in); err == nil {
			t.Fatal("private import without a user was not rejected")
		}
		if _, err := service.ImportWorkspace(owner, "test", in); err != nil {
			t.Fatal(err)
		}
		if _, err := service.ProjectContext(outsider, "import"); !errors.Is(err, ErrNotFound) {
			t.Fatal("source member retained access", err)
		}
		st, err := service.State(owner)
		if err != nil {
			t.Fatal(err)
		}
		if got := productByID(st, "import"); got == nil || got.Owner != "owner" || got.Visibility != "private" || len(got.Members) != 0 {
			t.Fatal(got)
		}
	})
}

func TestPrivateDeletedHistoryAndCreationAudit(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		service := New(store)
		owner := userCtx("owner", "human", "", "admin")
		outsider := userCtx("outsider", "human", "", "admin")
		bootstrap := userCtx("service/bootstrap", "service", "", "admin")
		privateReviewCreate(t, service, owner, "private-audit", "private")
		st, err := service.State(owner)
		if err != nil || len(st.Events) != 1 || st.Events[0].ProductID != "private-audit" || st.Events[0].Data.ProductID != "private-audit" {
			t.Fatalf("bad creation event: %v %v", st.Events, err)
		}
		created := st.Events[0]
		if st, err = service.State(outsider); err != nil || len(st.Events) != 0 {
			t.Fatalf("creation event leaked: %v %v", st.Events, err)
		}
		if _, err := service.Execute(owner, domain.Command{Action: "delete_product", ProductID: "private-audit", Data: map[string]any{"name": "name-private-audit"}}); err != nil {
			t.Fatal(err)
		}
		if err := store.Update(bootstrap, func(st *domain.State) error {
			st.Memories = append(st.Memories, domain.Memory{Meta: domain.Meta{ID: "retained-memory", ProductID: "private-audit"}, Kind: "progress", Body: "retained private information"})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, caller := range []context.Context{owner, outsider} {
			st, err = service.State(caller)
			if err != nil || len(st.Events) != 0 || len(st.Memories) != 0 {
				t.Fatalf("deleted history leaked: %v %v %v", st.Events, st.Memories, err)
			}
			if err := service.authorizeReferences(caller, "read", created.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("retained event reference was visible", err)
			}
			if _, indexed := store.(contextRetrievalReader); indexed {
				if _, err := service.ContextExcerpt(caller, domain.ContextExcerptRequest{ProductID: "private-audit", ID: "retained-memory"}); !errors.Is(err, ErrNotFound) {
					t.Fatal("retained memory excerpt was visible", err)
				}
				if _, err := service.GraphNeighbors(caller, domain.GraphNeighborsRequest{ProductID: "private-audit", ID: "retained-memory"}); !errors.Is(err, ErrNotFound) {
					t.Fatal("retained memory graph was visible", err)
				}
			}
			archive, err := service.CreateBackup(caller, "test")
			if err != nil {
				t.Fatal("deleted history leaked via backup", err)
			}
			if snapshot := privateReviewBackup(t, archive); len(snapshot.Events) != 0 || len(snapshot.Memories) != 0 {
				t.Fatal("deleted history leaked via backup")
			}
		}
		archive, err := service.CreateBackup(bootstrap, "test")
		if err != nil {
			t.Fatal("bootstrap did not retain audit history", err)
		}
		if snapshot := privateReviewBackup(t, archive); len(snapshot.Events) != 2 || len(snapshot.Memories) != 1 {
			t.Fatal("bootstrap backup did not retain history")
		}
		if _, err := service.Execute(outsider, domain.Command{Action: "create_product", ID: "private-audit", Data: map[string]any{"name": "Reuse"}}); !errors.Is(err, ErrNotFound) {
			t.Fatal("retired Product ID was disclosed or reused", err)
		}
	})
}

func privateReviewBackup(t *testing.T, archive Backup) domain.State {
	t.Helper()
	compressed, err := base64.StdEncoding.DecodeString(archive.ArchiveBase64)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var envelope backupEnvelope
	if err := json.NewDecoder(reader).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.State
}

func TestPrivateRestoreEvidence(t *testing.T) {
	ctx := context.Background()
	owner := userCtx("owner", "human", "", "admin")
	source := &memoryStore{domain.EmptyState()}
	service := New(source)
	privateReviewCreate(t, service, owner, "secret", "private")
	meta := func(key string) domain.Meta { return domain.Meta{ID: key, ProductID: "secret"} }
	if err := source.Update(ctx, func(st *domain.State) error {
		st.Operations = append(st.Operations, domain.ExternalOperation{Meta: meta("original-operation"), Kind: "DEPLOY", Status: "PENDING"})
		st.Builds = append(st.Builds, domain.Build{Meta: meta("original-build"), Status: "RUNNING", SigningStatus: "PENDING"})
		st.Deployments = append(st.Deployments, domain.Deployment{Meta: meta("original-deploy"), Status: "PENDING_CHECKS"})
		st.ArtifactSignatures = append(st.ArtifactSignatures, domain.ArtifactSignature{Meta: meta("original-signature"), Status: "PENDING"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	archive, err := service.CreateBackup(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	privateReviewStores(t, func(t *testing.T, store Store) {
		dest := New(store)
		if _, err := dest.RestoreBackup(ctx, "test", archive.ArchiveBase64, archive.SHA256); err != nil {
			t.Fatal(err)
		}
		outsider, err := dest.State(userCtx("outsider", "human", "", "admin"))
		if err != nil || len(outsider.Events) != 1 || outsider.Events[0].Action != "restore_backup" {
			t.Fatalf("global audit contains unexpected evidence: %v %v", outsider.Events, err)
		}
		if raw, _ := json.Marshal(outsider.Events[0]); strings.Contains(string(raw), "original-") {
			t.Fatal(string(raw))
		}
		visible, err := dest.State(owner)
		if err != nil || len(visible.Events) != 6 {
			t.Fatalf("incomplete journal: %v %v", visible.Events, err)
		}
		wanted := map[string]string{"original-operation": "PENDING", "original-build": "RUNNING", "original-deploy": "PENDING_CHECKS", "original-signature": "PENDING"}
		for _, event := range visible.Events {
			if event.Action != "restore_backup_evidence" {
				continue
			}
			var original struct{ ID, Status, SigningStatus string }
			raw, _ := json.Marshal(event.Data.Data["record"])
			if err := json.Unmarshal(raw, &original); err != nil || original.ID != event.EntityID || wanted[original.ID] != original.Status || event.ProductID != "secret" || event.Data.Data["restore_event_id"] != outsider.Events[0].ID {
				t.Fatalf("wrong original restore record: %s, %v", raw, event)
			}
			delete(wanted, original.ID)
		}
		if len(wanted) != 0 {
			t.Fatal(wanted)
		}
	})
}

type privateBranchProvider struct {
	delivery.Provider
	locators []string
}

func (p *privateBranchProvider) Branches(_ context.Context, _ delivery.Connection, locator string) ([]delivery.BranchInfo, error) {
	p.locators = append(p.locators, locator)
	return []delivery.BranchInfo{{Name: "main"}}, nil
}

func TestPrivateBranchLookupUsesRegistryLocator(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		provider := &privateBranchProvider{}
		service := NewWithProvider(store, provider)
		owner := userCtx("owner", "human", "", "member")
		privateReviewCreate(t, service, owner, "secret", "private")
		if _, err := service.Execute(owner, domain.Command{Action: "add_product_member", ProductID: "secret", Data: map[string]any{"user_id": "member"}}); err != nil {
			t.Fatal(err)
		}
		if err := store.Update(context.Background(), func(st *domain.State) error {
			st.Repositories = append(st.Repositories, domain.Repository{Meta: domain.Meta{ID: "repo", ProductID: "secret"}, Role: "APPLICATION", RegisteredRepositoryID: "registry"})
			st.RegisteredRepositories = append(st.RegisteredRepositories, domain.RegisteredRepository{Meta: domain.Meta{ID: "registry"}, ProviderRepositoryID: 1234, FullName: "new/repo"})
			st.RepositoryBindings = append(st.RepositoryBindings, domain.RepositoryBinding{Meta: domain.Meta{ID: "binding", ProductID: "secret"}, Role: "APPLICATION", RepositoryID: "repo", ConnectionID: "conn", FullName: "old/repo"})
			st.GitHubConnections = append(st.GitHubConnections, domain.GitHubConnection{Meta: domain.Meta{ID: "conn"}})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		member := userCtx("member", "human", "", "member")
		if st, err := service.State(member); err != nil || len(st.RegisteredRepositories) != 0 || len(st.GitHubConnections) != 0 {
			t.Fatal("instance registry leaked", err)
		}
		if value, err := service.Query(member, "list_branches", "repo"); err != nil || len(value.([]delivery.BranchInfo)) != 1 {
			t.Fatal(value, err)
		}
		if _, err := service.Query(userCtx("outsider", "human", "", "admin"), "list_branches", "repo"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if len(provider.locators) != 1 || provider.locators[0] != "1234" {
			t.Fatal(provider.locators)
		}
	})
}

func TestPrivateUIStateStaleSelection(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		if _, ok := store.(uiStateReader); !ok {
			return
		}
		service := New(store)
		owner := userCtx("owner", "human", "", "member")
		privateReviewCreate(t, service, owner, "secret", "private")
		privateReviewCreate(t, service, owner, "shared", "shared")
		if _, err := service.Execute(owner, domain.Command{Action: "create_feature", ID: "secret-feature", ProductID: "secret", Data: map[string]any{"title": "Secret", "goal": "Secret"}}); err != nil {
			t.Fatal(err)
		}
		reader := auth.WithPrincipal(context.Background(), auth.Principal{ID: "reader", Kind: "human", Products: []string{"shared"}, Permissions: []string{"read"}})
		for _, selection := range [][2]string{{"secret", ""}, {"", "secret-feature"}, {"secret", "secret-feature"}} {
			if _, err := service.UIState(reader, selection[0], selection[1]); !errors.Is(err, ErrNotFound) {
				t.Fatal(selection, err)
			}
		}
		if view, err := service.UIState(reader, "", ""); err != nil || view.Projection.ProductID != "shared" || len(view.Products) != 1 {
			t.Fatalf("default view is not visible: %v %v", view.Projection, err)
		}
	})
}

func TestPrivateInfrastructureServiceProductGrants(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		service := New(store)
		owner := userCtx("owner", "human", "", "admin")
		privateReviewCreate(t, service, owner, "allowed", "private")
		privateReviewCreate(t, service, owner, "outside", "private")
		principal := auth.WithPrincipal(context.Background(), auth.Principal{ID: "service/custom", Kind: "service", Products: []string{"allowed"}, Permissions: []string{"read", "write"}})
		if st, err := service.State(principal); err != nil || len(st.Products) != 1 || st.Products[0].ID != "allowed" {
			t.Fatalf("service product visibility: %+v %v", st.Products, err)
		}
		if _, ok := store.(stateMetadataReader); ok {
			if page, err := service.StatePage(principal, domain.StatePageRequest{}); err != nil || len(page.Items) != 1 || page.Items[0].ID != "allowed" {
				t.Fatalf("service navigation scope: %+v %v", page.Items, err)
			}
		}
		command := domain.Command{Action: "create_feature", ID: "infra-feature", ProductID: "allowed", Data: map[string]any{"title": "Infrastructure feature", "goal": "Test scoped writes"}}
		if _, err := service.Execute(principal, command); err != nil {
			t.Fatalf("service could not write to its allowed private Product: %v", err)
		}
		command.ID, command.ProductID = "outside-feature", "outside"
		if _, err := service.Execute(principal, command); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("out-of-scope write: got %v, want forbidden", err)
		}
	})
}

func TestPrivateUIStateScopedSelectedProductOutsideNavigationWindow(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		if _, ok := store.(uiStateReader); !ok {
			return
		}
		owner := userCtx("owner", "human", "", "member")
		const productID, featureID = "selected", "selected-feature"
		if err := store.Update(context.Background(), func(st *domain.State) error {
			st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: productID}, Name: "Selected", Description: "Full product record", Visibility: "private", Owner: "owner"})
			st.Features = append(st.Features, domain.Feature{Meta: domain.Meta{ID: featureID, ProductID: productID}, Title: "Selected feature", Status: "planned"})
			st.Repositories = append(st.Repositories, domain.Repository{Meta: domain.Meta{ID: "selected-repo", ProductID: productID}, Role: "APPLICATION"})
			for index := range 1001 {
				st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: fmt.Sprintf("newer-%04d", index)}, Name: "Newer"})
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		service := New(store)
		for _, scope := range [][2]string{{productID, ""}, {"", featureID}} {
			view, err := service.UIState(owner, scope[0], scope[1])
			if err != nil {
				t.Fatal(err)
			}
			if view.Projection.ProductID != productID || len(view.Products) != view.Projection.CollectionLimit || !slices.Contains(view.Projection.Truncated, "products") {
				t.Fatalf("unbounded or wrong projection: %+v; products = %d", view.Projection, len(view.Products))
			}
			product := productByID(view.State, productID)
			if product == nil || product.Description != "Full product record" || len(view.Features) != 1 || len(view.Repositories) != 1 {
				t.Fatalf("selected product or child records omitted: product=%+v features=%+v repositories=%+v", product, view.Features, view.Repositories)
			}
			if _, err = service.UIState(userCtx("outsider", "human", "", "admin"), scope[0], scope[1]); !errors.Is(err, ErrNotFound) {
				t.Fatalf("hidden selection: got %v, want NOT_FOUND", err)
			}
		}
	})
}

func TestPrivateCommandReferenceIDsIgnoreText(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		service := New(store)
		owner := userCtx("owner", "human", "", "member")
		privateReviewCreate(t, service, owner, "hidden", "private")
		privateReviewCreate(t, service, owner, "shared", "shared")
		exec(t, service, domain.Command{Action: "create_repository", ID: "hidden-repo", ProductID: "hidden", Data: map[string]any{"name": "Repository", "url": "https://github.com/test/repo"}})
		writer := auth.WithPrincipal(context.Background(), auth.Principal{ID: "writer", Kind: "human", Products: []string{"shared"}, Permissions: []string{"write"}})
		feature := domain.Command{Action: "create_feature", ID: "visible-feature", ProductID: "shared", Data: map[string]any{"title": "hidden", "goal": "hidden-repo", "requirements": []string{"hidden-repo"}}}
		if _, err := service.Execute(writer, feature); err != nil {
			t.Fatalf("text was treated as an ID: %v", err)
		}
		integration := domain.Command{Action: "create_integration", FeatureID: feature.ID, Data: map[string]any{"title": "Integration", "objective": "Test"}}
		for _, ref := range []map[string]any{{"repositories": []string{"hidden-repo"}}, {"branches": []domain.Branch{{RepositoryID: "hidden-repo"}}}} {
			for key, value := range ref {
				integration.Data[key] = value
				if _, err := service.Execute(writer, integration); !errors.Is(err, ErrNotFound) {
					t.Errorf("real reference %s: got %v, want NOT_FOUND", key, err)
				}
				delete(integration.Data, key)
			}
		}
	})
}

func TestPrivateRuntimeReporterPersistence(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		reports, ok := store.(rr.Store)
		if !ok {
			return
		}
		service := New(store)
		owner := userCtx("owner", "human", "", "member")
		privateReviewCreate(t, service, owner, "secret", "private")
		reporter := auth.WithPrincipal(context.Background(), auth.Principal{ID: "service/prod-runtime-reporter", Kind: "service"})
		report := rr.Report{ProductID: "secret", EnvironmentID: "prod", Cluster: "cluster", ReportedAt: time.Now().UTC()}
		if _, err := service.ReportRuntime(reporter, report); err != nil {
			t.Fatalf("report rejected: %v", err)
		}
		clusters, err := reports.ReadRuntimeReports(owner, report.ProductID, report.EnvironmentID, report.Cluster)
		if err != nil || len(clusters) != 1 || clusters[0].Cluster != report.Cluster {
			t.Fatalf("report not persisted: %v, %v", clusters, err)
		}
		if _, err := service.GetRuntimeReport(reporter, report.ProductID, report.EnvironmentID, report.Cluster); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("reporter unexpectedly authorized to read: %v", err)
		}
	})
}
