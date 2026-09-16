package transport_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
)

// This test stores execution evidence directly, avoiding external providers. It
// verifies the persistence/backup contract independently of worker execution.
func TestPostgresFlowHistoryRestartAndBackupMigration(t *testing.T) {
	ctx := context.Background()
	sourceDSN, destDSN := isolatedDatabase(t), isolatedDatabase(t)
	source, err := persistence.Open(ctx, sourceDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { source.Close() }()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	meta := func(id string) domain.Meta {
		return domain.Meta{ID: id, ProductID: "p", Actor: "agent", CreatedAt: now, UpdatedAt: now}
	}
	seed := domain.EmptyState()
	seed.Products = append(seed.Products, domain.Product{Meta: meta("p"), Name: "Portable product"})
	seed.Features = append(seed.Features, domain.Feature{Meta: meta("feature"), Title: "Payment", Status: "active", Goal: "Independent release"})
	integrationMeta := meta("integration")
	integrationMeta.FeatureID = "feature"
	seed.Integrations = append(seed.Integrations, domain.Integration{Meta: integrationMeta, Title: "Callback fix", Kind: "hotfix", Status: "released", Repositories: []string{"repository"}})
	seed.Repositories = append(seed.Repositories, domain.Repository{Role: "APPLICATION", Meta: meta("repository"), Name: "team/source"})
	seed.Applications = append(seed.Applications, domain.Application{Kind: "APPLICATION", Meta: meta("component"), Name: "API", RepositoryID: "repository"})
	seed.Environments = append(seed.Environments, domain.Environment{Meta: meta("environment"), Name: "TEST", Cluster: "cluster", Namespace: "test", DesiredCompositionID: "composition", DesiredOperationID: "active-parent"})
	revision := domain.IntegrationRevision{Meta: meta("revision"), IntegrationID: "integration", RepositoryID: "repository", Branch: "feature/fix", BaseCommit: deliveryBase, HeadCommit: deliveryHead, Commits: []string{deliveryHead}}
	seed.IntegrationRevisions = append(seed.IntegrationRevisions, revision)
	composition := domain.Composition{Meta: meta("composition"), Name: "Selected work", Status: "planned", EnvironmentID: "environment", Components: []domain.CompositionComponent{{ApplicationID: "component", BaseRef: "main", BaseCommit: deliveryBase, RevisionIDs: []string{"revision"}}}, RevisionSnapshots: []domain.IntegrationRevision{revision}, ApplicationSnapshots: seed.Applications, EnvironmentSnapshot: seed.Environments[0]}
	seed.Compositions = append(seed.Compositions, composition)
	snapshot := domain.DeliverySnapshot{Revision: revision, Application: seed.Applications[0], Environment: seed.Environments[0], BuildRequest: delivery.BuildRequest{OperationID: "built-child", SourceSHA: deliveryHead, WorkflowSHA: deliveryBase, Inputs: map[string]string{"source_sha": deliveryHead, "operation_id": "built-child"}}}
	sources := []domain.FlowSource{{RepositoryID: "repository", Plan: delivery.SourcePlan{Repository: "11", BaseRef: "main", BaseSHA: deliveryBase, TargetBranch: "generated/test/op", HeadSHAs: []string{deliveryHead}, OperationID: "candidate"}, Result: &delivery.SourceResult{Branch: "generated/test/op", SHA: deliveryHead}, MainAdvanced: true}}
	seed.Operations = append(seed.Operations,
		domain.ExternalOperation{Meta: meta("candidate"), Kind: "RELEASE_CANDIDATE", Status: "SUCCEEDED", DeploymentState: "READY_FOR_VERIFICATION", ChildIDs: []string{"built-child"}, IntegrationIDs: []string{"integration"}, CompositionSnapshot: &composition, Sources: sources, PreparedDeployments: []domain.DeliverySnapshot{snapshot}},
		domain.ExternalOperation{Meta: meta("built-child"), Kind: "DEPLOY", Status: "SUCCEEDED", BuildOnly: true, DeploymentState: "ARTIFACT_READY", ParentID: "candidate", ApplicationID: "component", EnvironmentID: "environment", Snapshot: &snapshot, Artifact: &delivery.ArtifactResult{Available: true, Digest: deliveryDigest}, Run: &delivery.RunResult{ID: 77, Found: true, Conclusion: "success"}},
		domain.ExternalOperation{Meta: meta("active-parent"), Kind: "COMPOSE", Status: "RUNNING", Phase: "WAIT_CHILDREN", ChildIDs: []string{"active-child"}, CompositionSnapshot: &composition, Sources: sources, PreparedDeployments: []domain.DeliverySnapshot{snapshot}, LeaseOwner: "old-worker", LeaseUntil: now.Add(time.Hour)},
		domain.ExternalOperation{Meta: meta("active-child"), Kind: "DEPLOY", Status: "PENDING", Phase: "BUILD_LOOKUP", ParentID: "active-parent", Snapshot: &snapshot, EnvironmentID: "environment", ApplicationID: "component"},
	)
	seed.Operations = append(seed.Operations,
		domain.ExternalOperation{Meta: meta("historical-promotion"), Kind: "RELEASE_PROMOTION", Status: "SUCCEEDED", DeploymentState: "DEPLOYED", ReleaseCandidateID: "candidate", ChildIDs: []string{"deployed-child"}, CompositionSnapshot: &composition, Sources: sources},
		domain.ExternalOperation{Meta: meta("deployed-child"), Kind: "DEPLOY", Status: "SUCCEEDED", DeploymentState: "GITOPS_APPLIED", ParentID: "historical-promotion", Snapshot: &snapshot, EnvironmentID: "environment", ApplicationID: "component", Artifact: &delivery.ArtifactResult{Available: true, Digest: deliveryDigest}, GitOpsResult: &delivery.GitOpsResult{CommitSHA: deliveryBase}},
	)
	for index, id := range []string{"z-first-version", "a-second-version"} {
		seed.ScenarioVersions = append(seed.ScenarioVersions, domain.ScenarioVersion{Meta: meta(id), ScenarioID: "z-first-version", Version: index + 1, Title: "No duplicate callback", Objective: "Exactly once", IntegrationIDs: []string{"integration"}, Steps: []string{"retry callback"}, ExpectedOutcomes: []string{"one payment"}, Mechanism: "integration", Blocking: true})
	}
	for _, id := range []string{"z-first-run", "a-second-run"} {
		seed.ScenarioRuns = append(seed.ScenarioRuns, domain.ScenarioRun{Meta: meta(id), ScenarioVersionID: "a-second-version", CandidateOperationID: "candidate", Result: "passed", Observations: []string{"one payment"}, Components: []domain.ScenarioComponentEvidence{{ApplicationID: "component", OperationID: "built-child", SourceSHA: deliveryHead, ArtifactDigest: deliveryDigest}}})
	}
	seed.RuntimeObservations = append(seed.RuntimeObservations, domain.RuntimeObservation{Meta: meta("runtime"), OperationID: "deployed-child", EnvironmentID: "environment", GitOpsCommit: deliveryBase, ArtifactDigest: deliveryDigest, Healthy: true, Details: "executor logs retained"})
	seed.Releases = append(seed.Releases, domain.Release{Meta: meta("release"), Name: "First release", Status: "released", ExecutionOperationID: "historical-promotion", CandidateOperationID: "candidate", IntegrationIDs: []string{"integration"}, Snapshots: seed.Integrations, SourceCommits: map[string]string{"component": deliveryHead}, ArtifactDigests: map[string]string{"component": deliveryDigest}, GitOpsCommits: map[string]string{"component": deliveryBase}})
	seed.OperationSteps = append(seed.OperationSteps, domain.OperationStep{Meta: meta("original-step"), OperationID: "active-parent", Phase: "QUEUE_COMPONENTS", Status: "SUCCEEDED", Evidence: map[string]string{"source_sha": deliveryHead}})
	if err = source.Update(ctx, func(st *domain.State) error { *st = seed; return nil }); err != nil {
		t.Fatal(err)
	}
	before, err := source.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	source, err = persistence.Open(ctx, sourceDSN)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := source.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, reopened) {
		t.Fatal("flow history or ordering changed after restart")
	}
	backup, err := application.New(source).CreateBackup(ctx, "agent/migrate")
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := base64.StdEncoding.DecodeString(backup.ArchiveBase64)
	if err != nil || len(compressed) < 2 || compressed[0] != 0x1f || compressed[1] != 0x8b {
		t.Fatal("backup is not gzip")
	}
	dest, err := persistence.Open(ctx, destDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { dest.Close() }()
	if _, err = application.New(dest).RestoreBackup(ctx, "agent/migrate", backup.ArchiveBase64, backup.SHA256); err != nil {
		t.Fatal(err)
	}
	dest.Close()
	dest, err = persistence.Open(ctx, destDSN)
	if err != nil {
		t.Fatal(err)
	}
	after, err := dest.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Events) != len(before.Events)+1 || len(after.OperationSteps) != len(before.OperationSteps)+2 {
		t.Fatal("migration cancellation audit missing")
	}
	rawInterrupted, _ := json.Marshal(after.Events[len(after.Events)-1].Data.Data["interrupted_operations"])
	var interrupted []domain.ExternalOperation
	if err = json.Unmarshal(rawInterrupted, &interrupted); err != nil || len(interrupted) != 2 || !reflect.DeepEqual(interrupted[0], before.Operations[2]) || !reflect.DeepEqual(interrupted[1], before.Operations[3]) {
		t.Fatal("pre-restore parent and child history was not retained")
	}
	for i := range after.Operations {
		original := before.Operations[i]
		if after.Operations[i].ID != original.ID {
			t.Fatal("operation order changed")
		}
		if original.Status == "PENDING" || original.Status == "RUNNING" {
			restored := after.Operations[i]
			if restored.Status != "CANCELLED" || restored.FinishedAt == nil || restored.LeaseOwner != "" || !restored.LeaseUntil.IsZero() {
				t.Fatal("active operation not safely cancelled")
			}
			// Only cancellation lifecycle fields may differ; frozen inputs and provider
			// outputs must survive byte-for-byte through PostgreSQL and gzip restore.
			restored.Status = original.Status
			restored.FinishedAt = original.FinishedAt
			restored.LeaseOwner = original.LeaseOwner
			restored.LeaseUntil = original.LeaseUntil
			restored.Error = original.Error
			restored.Detail = original.Detail
			restored.UpdatedAt = original.UpdatedAt
			if !reflect.DeepEqual(restored, original) {
				t.Fatal("cancelled operation lost immutable execution evidence")
			}
		} else if !reflect.DeepEqual(after.Operations[i], original) {
			t.Fatal("terminal operation changed")
		}
	}
	normalized := after
	normalized.Operations = before.Operations
	normalized.Events = before.Events
	normalized.OperationSteps = before.OperationSteps
	if !reflect.DeepEqual(before, normalized) {
		a, _ := json.Marshal(before)
		b, _ := json.Marshal(normalized)
		t.Fatalf("migration changed flow records\nbefore=%s\nafter=%s", a, b)
	}
	provider := &persistedDeliveryFixture{}
	worked, err := application.NewWithProvider(dest, provider).Tick(ctx)
	if err != nil || worked || provider.dispatches.Load() != 0 || provider.applies.Load() != 0 {
		t.Fatal("restore replayed external work")
	}
}
