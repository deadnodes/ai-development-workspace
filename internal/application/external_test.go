package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

type fakeDelivery struct {
	compareBase              string
	extraBranches            []delivery.BranchInfo
	publicPresent            bool
	behind                   int
	dispatches, applies      int
	dispatchError, applyLost bool
	currentDigest            string
	branchSHA                string
}

func (f *fakeDelivery) TestConnection(context.Context, delivery.Connection) error { return nil }
func (f *fakeDelivery) DiscoverRepositories(context.Context, delivery.Connection) ([]delivery.RepositoryInfo, error) {
	return []delivery.RepositoryInfo{{ID: 42, FullName: "owner/source", DefaultBranch: "main"}, {ID: 43, FullName: "owner/gitops", DefaultBranch: "main"}}, nil
}
func (f *fakeDelivery) Branches(context.Context, delivery.Connection, string) ([]delivery.BranchInfo, error) {
	sha := f.branchSHA
	if sha == "" {
		sha = shaHead
	}
	return append([]delivery.BranchInfo{{Name: "feature/work", SHA: sha}, {Name: "main", SHA: shaBase}}, f.extraBranches...), nil
}
func (f *fakeDelivery) Compare(_ context.Context, _ delivery.Connection, _ string, base string, _ string) (delivery.CompareResult, error) {
	f.compareBase = base
	return delivery.CompareResult{Ahead: 1, Behind: f.behind, Status: "ahead"}, nil
}

func (f *fakeDelivery) Dispatch(context.Context, delivery.Connection, delivery.BuildRequest) (delivery.DispatchResult, error) {
	f.dispatches++
	if f.dispatchError {
		return delivery.DispatchResult{}, errors.New("response lost")
	}
	return delivery.DispatchResult{ProviderRunID: 12}, nil
}
func (f *fakeDelivery) FindRun(context.Context, delivery.Connection, delivery.BuildRequest) (delivery.RunResult, error) {
	return delivery.RunResult{Found: true, ID: 12, Status: "completed", Conclusion: "success", HeadSHA: shaBase, Artifacts: map[string]string{"digest": "sha256:" + strings.Repeat("d", 64), "source_sha": shaHead, "image_repository": "ghcr.io/owner/component"}, URL: "https://example.test/run"}, nil
}
func (f *fakeDelivery) InspectArtifact(_ context.Context, _ delivery.Connection, req delivery.ArtifactRequest) (delivery.ArtifactResult, error) {
	if req.EvidenceRunID == 0 && f.publicPresent {
		return delivery.ArtifactResult{Available: true, Digest: "sha256:" + strings.Repeat("d", 64), ObservedAt: time.Now().UTC()}, nil
	}
	if req.EvidenceRunID == 0 {
		return delivery.ArtifactResult{Available: false, ObservedAt: time.Now().UTC()}, nil
	}
	return delivery.ArtifactResult{Available: true, Digest: "sha256:" + strings.Repeat("d", 64), ObservedAt: time.Now().UTC()}, nil
}

func (f *fakeDelivery) ReadGitOps(context.Context, delivery.Connection, delivery.GitOpsRequest) (delivery.GitOpsSnapshot, error) {
	head, blob := "gitops-base", "blob-base"
	if f.currentDigest != "" {
		head = "gitops-applied"
		blob = "blob-applied"
	}
	return delivery.GitOpsSnapshot{ImageRepository: "ghcr.io/owner/component", HeadSHA: head, BlobSHA: blob, Digest: f.currentDigest}, nil
}
func (f *fakeDelivery) ApplyGitOps(_ context.Context, _ delivery.Connection, a delivery.GitOpsApply) (delivery.GitOpsResult, error) {
	f.applies++
	if a.ExpectedHeadSHA != "gitops-base" || a.ExpectedBlobSHA != "blob-base" {
		return delivery.GitOpsResult{}, errors.New("CAS mismatch")
	}
	f.currentDigest = a.Digest
	if f.applyLost {
		return delivery.GitOpsResult{}, errors.New("commit response lost")
	}
	return delivery.GitOpsResult{CommitSHA: "gitops-applied", BlobSHA: "blob-applied"}, nil
}
func externalFixture(t *testing.T) (*Service, *memoryStore, *fakeDelivery) {
	t.Helper()
	s, m := fixture(t)
	f := &fakeDelivery{}
	s = NewWithProvider(m, f)
	exec(t, s, domain.Command{Action: "create_github_connection", ID: "conn", ProductID: "p", Data: map[string]any{"name": "GitHub", "app_id": 1, "installation_id": 2, "private_key_ref": "env:TEST_APP_KEY", "owner": "owner"}})
	if _, err := s.Query(context.Background(), "discover_repositories", "conn"); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []struct{ id, role string }{{"source", "SOURCE"}, {"gitops", "GITOPS"}} {
		exec(t, s, domain.Command{Action: "import_repository", ID: repo.id, ProductID: "p", Data: map[string]any{"connection_id": "conn", "full_name": "owner/" + repo.id, "role": repo.role, "default_branch": "main"}})
	}
	exec(t, s, domain.Command{Action: "create_application", ID: "component", ProductID: "p", Data: map[string]any{"name": "Component", "repository_id": "source"}})
	exec(t, s, domain.Command{Action: "create_environment", ID: "dev", ProductID: "p", Data: map[string]any{"name": "Friendly environment", "cluster": "local", "namespace": "dev"}})
	exec(t, s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component", "connection_id": "conn", "workflow": "build.yml", "rebuild_missing": true, "image_repository": "ghcr.io/owner/component"}})
	exec(t, s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "app.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "rev", IntegrationID: "i", Data: map[string]any{"repository_id": "source", "branch": "feature/work", "base_commit": shaBase, "head_commit": shaHead, "commits": []string{shaHead}}})
	return s, m, f
}
func deployCommand() domain.Command {
	return domain.Command{Action: "deploy_integration", ID: "deploy", IntegrationID: "i", Data: map[string]any{"environment_id": "dev", "application_id": "component", "revision_id": "rev"}}
}
func tickNow(t *testing.T, s *Service, m *memoryStore) {
	t.Helper()
	for i := range m.state.Operations {
		m.state.Operations[i].NextAttemptAt = time.Time{}
		m.state.Operations[i].LeaseUntil = time.Time{}
	}
	worked, e := s.Tick(context.Background())
	if e != nil || !worked {
		t.Fatalf("tick: worked=%v err=%v", worked, e)
	}
}
func runDelivery(t *testing.T, s *Service, m *memoryStore) {
	t.Helper()
	for range 10 {
		if terminalOperation(m.state.Operations[0].Status) {
			return
		}
		tickNow(t, s, m)
	}
	t.Fatal("operation did not finish")
}
func TestDurableDeliveryPinsInputsAndStopsBeforeReconciliation(t *testing.T) {
	s, m, f := externalFixture(t)
	exec(t, s, deployCommand())
	if len(m.state.Operations) != 1 || f.dispatches != 0 {
		t.Fatal("HTTP command performed network work")
	}
	exec(t, s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component", "connection_id": "conn", "workflow": "new.yml", "image_repository": "ghcr.io/owner/new"}})
	tickNow(t, s, m)
	s = NewWithProvider(m, f)
	runDelivery(t, s, m)
	op := m.state.Operations[0]
	if op.Status != "SUCCEEDED" || op.DeploymentState != "GITOPS_APPLIED" || op.Snapshot.Build.Workflow != "build.yml" || op.Snapshot.BuildRequest.SourceSHA != shaHead || f.dispatches != 1 || f.applies != 1 {
		t.Fatalf("wrong result: %+v", op)
	}
	if len(m.state.Environments[0].Runtime) != 0 || len(m.state.Environments[0].Reconciled) != 0 {
		t.Fatal("claimed observed deployment")
	}
	if len(m.state.OperationSteps) < 6 {
		t.Fatal("step history absent")
	}
}
func TestUncertainDispatchAndApplyRecoverWithoutDuplicates(t *testing.T) {
	s, m, f := externalFixture(t)
	f.dispatchError = true
	f.applyLost = true
	exec(t, s, deployCommand())
	runDelivery(t, s, m)
	if m.state.Operations[0].Status != "SUCCEEDED" || f.dispatches != 1 || f.applies != 1 {
		t.Fatalf("duplicate or failed operation: %+v", m.state.Operations[0])
	}
}
func TestRestartAfterDispatchIntentOnlyFindsExistingRun(t *testing.T) {
	s, m, f := externalFixture(t)
	exec(t, s, deployCommand())
	tickNow(t, s, m)
	m.state.Operations[0].Phase = "BUILD_LOOKUP"
	m.state.Operations[0].Status = "BUILDING"
	m.state.Operations[0].LeaseOwner = "dead-worker"
	m.state.Operations[0].LeaseUntil = time.Now().Add(-time.Minute)
	s = NewWithProvider(m, f)
	runDelivery(t, s, m)
	if f.dispatches != 0 || m.state.Operations[0].Status != "SUCCEEDED" {
		t.Fatal("restart blindly redispatched")
	}
}
func TestDeliveryBlocksSourceDriftAndRequiresExplicitPolicy(t *testing.T) {
	s, m, f := externalFixture(t)
	f.branchSHA = strings.Repeat("c", 40)
	exec(t, s, deployCommand())
	tickNow(t, s, m)
	if m.state.Operations[0].DeploymentState != "BLOCKED" || f.dispatches != 0 {
		t.Fatal("source drift not blocked")
	}
	exec(t, s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component", "purpose": "PROD", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "app.yaml", "image_field": "image.repository", "allow_deploy": true}})
	reject(t, s, deployCommand()) // PROD requires verified main candidate, never direct integration deployment.
	reject(t, s, domain.Command{Action: "create_github_connection", ProductID: "p", Data: map[string]any{"name": "bad", "app_id": 1, "installation_id": 2, "private_key_ref": "-----BEGIN PRIVATE KEY-----", "owner": "owner"}})
}
func TestGitObservationDoesNotModifyBranchOrCommits(t *testing.T) {
	s, m, f := externalFixture(t)
	f.behind = 2
	exec(t, s, domain.Command{Action: "refresh_integration_git", IntegrationID: "i"})
	tickNow(t, s, m)
	if len(m.state.GitObservations) != 1 || m.state.GitObservations[0].Behind != 2 || f.dispatches != 0 || len(m.state.Integrations[0].Commits) != 0 {
		t.Fatal("observation changed source state")
	}
}

func TestInstanceRegistrySharedAcrossProductsAndConnections(t *testing.T) {
	s, m, _ := externalFixture(t)
	exec(t, s, domain.Command{Action: "create_product", ID: "second", Data: map[string]any{"name": "Second"}})
	exec(t, s, domain.Command{Action: "create_github_connection", ID: "second-conn", ProductID: "second", Data: map[string]any{"name": "Second app", "app_id": 3, "installation_id": 4, "private_key_ref": "env:OTHER_KEY", "owner": "owner"}})
	exec(t, s, domain.Command{Action: "import_repository", ID: "second-source", ProductID: "second", Data: map[string]any{"connection_id": "second-conn", "full_name": "owner/source", "role": "SOURCE", "default_branch": "main"}})
	if len(m.state.RegisteredRepositories) != 2 {
		t.Fatalf("same instance repository duplicated: %d", len(m.state.RegisteredRepositories))
	}
	var first, second string
	for _, r := range m.state.Repositories {
		if r.ID == "source" {
			first = r.RegisteredRepositoryID
		}
		if r.ID == "second-source" {
			second = r.RegisteredRepositoryID
		}
	}
	if first == "" || first != second {
		t.Fatal("product attachments do not share registry identity")
	}
	for _, c := range m.state.GitHubConnections {
		if c.ProductID != "" {
			t.Fatal("instance connection owned exclusively by product")
		}
	}
	reject(t, s, domain.Command{Action: "import_repository", ProductID: "second", Data: map[string]any{"connection_id": "conn", "full_name": "owner/other", "role": "SOURCE", "default_branch": "main"}})
	exec(t, s, domain.Command{Action: "grant_connection", ProductID: "second", Data: map[string]any{"connection_id": "conn"}})
}
func TestDeployCapturesBoundBranchWithoutManualSHA(t *testing.T) {
	s, m, _ := externalFixture(t)
	exec(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"branches": []domain.Branch{{RepositoryID: "source", Name: "feature/work"}}}})
	exec(t, s, domain.Command{Action: "deploy_integration", ID: "auto", IntegrationID: "i", Data: map[string]any{"environment_id": "dev"}})
	runDelivery(t, s, m)
	op := m.state.Operations[0]
	if op.Status != "SUCCEEDED" || op.Snapshot.Revision.ID == "" || op.Snapshot.Revision.HeadCommit != shaHead || op.Snapshot.BuildRequest.WorkflowSHA != shaBase || op.Snapshot.BuildRequest.ImageTag != "rcp-"+shaHead {
		t.Fatalf("automatic pinning failed: %+v", op)
	}
}
func TestDeploymentPolicyChangeStopsPinnedOperation(t *testing.T) {
	s, m, f := externalFixture(t)
	exec(t, s, deployCommand())
	for m.state.Operations[0].Phase != "APPLY" {
		tickNow(t, s, m)
	}
	exec(t, s, domain.Command{Action: "update_environment", ID: "dev", Data: map[string]any{"namespace": "changed"}})
	tickNow(t, s, m)
	if m.state.Operations[0].DeploymentState != "BLOCKED" || f.applies != 0 {
		t.Fatal("stale target policy allowed mutation")
	}
}
func TestRegistryNumericIdentitySurvivesRename(t *testing.T) {
	s, m, _ := externalFixture(t)
	registry := registeredRepository(&m.state, "conn", "owner/source")
	registry.ProviderRepositoryID = 42
	if repositoryLocator(&m.state, *repositoryBinding(&m.state, "source")) != "42" {
		t.Fatal("provider numeric identity not used")
	}
	registry.FullName = "owner/renamed"
	if found := registeredRepositoryIdentity(&m.state, "conn", "owner/newname", 42); found == nil || found.ID != registry.ID {
		t.Fatal("rename lost registry identity")
	}
	_ = s
}

func TestArtifactTagAloneCannotProveSourceButTrustedDigestIsReused(t *testing.T) {
	s, m, f := externalFixture(t)
	f.publicPresent = true
	exec(t, s, deployCommand())
	runDelivery(t, s, m)
	if f.dispatches != 1 {
		t.Fatal("unverified public tag skipped source attestation")
	}
	cmd := deployCommand()
	cmd.ID = "reuse"
	exec(t, s, cmd)
	for range 8 {
		if terminalOperation(m.state.Operations[1].Status) {
			break
		}
		tickNow(t, s, m)
	}
	if m.state.Operations[1].Status != "SUCCEEDED" || f.dispatches != 1 {
		t.Fatal("known source/digest artifact was not reused")
	}
}
func TestSchedulerSelectsEarliestEligibleOperation(t *testing.T) {
	s, m, _ := externalFixture(t)
	exec(t, s, domain.Command{Action: "refresh_integration_git", ID: "late", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "refresh_integration_git", ID: "early", IntegrationID: "i"})
	m.state.Operations[0].NextAttemptAt = time.Now().Add(-time.Minute)
	m.state.Operations[1].NextAttemptAt = time.Now().Add(-2 * time.Minute)
	worked, e := s.Tick(context.Background())
	if e != nil || !worked {
		t.Fatal(e)
	}
	if m.state.Operations[1].Status != "SUCCEEDED" || m.state.Operations[0].Status != "PENDING" {
		t.Fatal("oldest record monopolized scheduling")
	}
}

func TestDisabledRebuildPolicyNeverDispatchesMissingArtifact(t *testing.T) {
	s, m, f := externalFixture(t)
	exec(t, s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component", "connection_id": "conn", "workflow": "build.yml", "image_repository": "ghcr.io/owner/component"}})
	exec(t, s, deployCommand())
	tickNow(t, s, m)
	if f.dispatches != 0 || m.state.Operations[0].DeploymentState != "BLOCKED" {
		t.Fatal("missing artifact built without explicit policy")
	}
}
func TestConfiguredBaseBranchDrivesComparison(t *testing.T) {
	s, m, f := externalFixture(t)
	alternate := strings.Repeat("e", 40)
	f.extraBranches = []delivery.BranchInfo{{Name: "release-base", SHA: alternate}}
	repositoryBinding(&m.state, "source").BaseBranch = "release-base"
	exec(t, s, deployCommand())
	tickNow(t, s, m)
	if f.compareBase != alternate {
		t.Fatal("provider default used instead of configured base")
	}
}

func TestCallerDigestConstraintDoesNotProveArtifactSource(t *testing.T) {
	s, m, f := externalFixture(t)
	f.publicPresent = true
	cmd := deployCommand()
	cmd.Data["expected_digest"] = "sha256:" + strings.Repeat("d", 64)
	exec(t, s, cmd)
	runDelivery(t, s, m)
	if f.dispatches != 1 {
		t.Fatal("caller-supplied digest bypassed trusted source report")
	}
}
func TestOtherProductArtifactDoesNotGrantSourceProvenance(t *testing.T) {
	s, m, f := externalFixture(t)
	f.publicPresent = true
	m.state.DeliveryArtifacts = append(m.state.DeliveryArtifacts, domain.DeliveryArtifact{Meta: domain.Meta{ID: "foreign", ProductID: "other"}, ApplicationID: "component", RepositoryID: "source", SourceCommit: shaHead, ImageRepository: "ghcr.io/owner/component", Digest: "sha256:" + strings.Repeat("d", 64), BuildRunID: 12, Availability: "PRESENT"})
	exec(t, s, deployCommand())
	runDelivery(t, s, m)
	if f.dispatches != 1 {
		t.Fatal("foreign product artifact supplied trusted provenance")
	}
}
