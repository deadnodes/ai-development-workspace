package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/intelligencepatch"
	"releasecontrol/internal/workerconfig"
)

func patchTaskFixture(t *testing.T) (*Service, *memoryStore, *intelligenceFixtureProvider, domain.ExternalOperation, domain.Command) {
	t.Helper()
	s, m, f := intelligenceFixture(t, 10)
	exec(t, s, domain.Command{Action: "create_repository", ID: "repo", ProductID: "p", Data: map[string]any{"name": "repo", "url": "https://github.com/test/repo"}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "patch-revision", IntegrationID: "i", Data: map[string]any{"repository_id": "repo", "branch": "feature", "base_commit": strings.Repeat("a", 40), "head_commit": strings.Repeat("b", 40), "commits": []string{strings.Repeat("b", 40)}}})
	m.state.CompositionConflicts = append(m.state.CompositionConflicts, domain.CompositionConflict{Meta: domain.Meta{ID: "conflict", ProductID: "p"}, Status: "requires_resolution", RepositoryID: "repo", OperationID: "conflicting-operation", Source: domain.FlowSource{RepositoryID: "repo", Plan: delivery.SourcePlan{BaseSHA: strings.Repeat("a", 40), HeadSHAs: []string{strings.Repeat("b", 40), strings.Repeat("c", 40)}}}, Integrations: []domain.Integration{m.state.Integrations[0]}})
	p := intelligencepatch.Proposal{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Edits: []intelligencepatch.Edit{{Path: "src/value.go", BeforeHash: intelligencepatchContentHash("package value\nconst A=1\n"), Content: "package value\nconst A=1\nconst B=2\n"}}}
	b, _ := json.Marshal(intelligenceResult{Decision: "patch_proposed", Summary: "Preserve both additions", Patch: &p})
	f.result = string(b)
	c := domain.Command{Action: "queue_intelligence_task", Actor: "agent", ProductID: "p", Data: map[string]any{"task_kind": "conflict_patch", "request_key": "request-conflict-1", "conflict_id": "conflict", "head_sha": p.HeadSHA, "files": []intelligenceFile{{Path: "src/value.go", Base: "package value\n", Ours: "package value\nconst A=1\n", Theirs: "package value\nconst B=2\n"}}}}
	op := exec(t, s, c).(domain.ExternalOperation)
	runIntelligenceTick(t, s)
	if operationByID(&m.state, op.ID).Status != "SUCCEEDED" {
		t.Fatal("patch task failed")
	}
	r := workerconfig.Recipe{ID: "patch-check", Revision: "v1", ProductID: "p", RepositoryID: "repo", RepositoryURL: "https://github.com/test/repo", Capability: "source_check", Trust: "trusted_source", Parameters: map[string]workerconfig.Parameter{}, IntelligencePaths: []string{"src/value.go"}, Execution: &domain.WorkerExecution{Kind: "source_check", ToolchainImage: "example/tool@sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64", CheckArgv: []string{"go", "test", "./..."}, Network: "none", TimeoutSeconds: 30, MemoryMB: 128, StorageMB: 16, CPUs: 1, PIDs: 64, SourceBytes: 1 << 20, OutputBytes: 1 << 20, LogBytes: 1 << 16}}
	r.Hash = workerconfig.RecipeHash(r)
	a, e := workerconfig.New(workerconfig.Config{Revision: "patch-config", LeaseSeconds: 30, Recipes: []workerconfig.Recipe{r}, Credentials: []workerconfig.Credential{{ID: "patch-worker", Principal: "worker/patch", TokenEnv: "WORKER_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"repo"}, Capabilities: []string{"source_check"}, ExpiresAt: time.Now().Add(time.Hour)}}}, "fixture-control-token", func(string) string { return workerTestToken })
	if e != nil {
		t.Fatal(e)
	}
	s.ConfigureWorkers(a)
	job := domain.Command{Action: "queue_worker_job", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: map[string]any{"intelligence_task_id": op.ID, "request_key": "request-patch-check-1", "recipe_id": r.ID, "recipe_revision": r.Revision, "recipe_hash": r.Hash, "config_revision": a.Revision(), "revision_id": "patch-revision", "source_sha": p.HeadSHA, "parameters": map[string]any{}, "deadline": time.Now().Add(time.Minute), "max_attempts": 1}}
	return s, m, f, op, job
}

func TestIntelligencePatchWorkerBindingAndNoAdmission(t *testing.T) {
	s, m, _, task, command := patchTaskFixture(t)
	j := exec(t, s, command).(domain.WorkerJob)
	stored, _ := json.Marshal(m.state)
	if strings.Contains(string(stored), "const B=2") {
		t.Fatal("patch plaintext leaked into persisted job/history")
	}
	p, e := s.externalWorkers.Authenticate(workerTestToken, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	lease, e := s.ClaimWorker(context.Background(), p, domain.WorkerClaim{ProductID: "p", RepositoryID: "repo", Capability: "source_check"})
	if e != nil || lease == nil || len(lease.IntelligencePatch) == 0 {
		t.Fatalf("no decrypted authenticated lease: %v", e)
	}
	if lease.Job.Inputs.IntelligencePatch.TaskID != task.ID {
		t.Fatal("patch task binding missing")
	}
	req := workerRequest(lease)
	if _, e = s.StartWorker(context.Background(), p, req); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ReportWorker(context.Background(), p, domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED"}); e == nil {
		t.Fatal("altered source passed without proposal fingerprint")
	}
	if _, e = s.ReportWorker(context.Background(), p, domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED", EvidenceHash: j.Inputs.IntelligencePatch.ResultFingerprint}); e != nil {
		t.Fatal(e)
	}
	q, e := s.Query(context.Background(), "get_intelligence_task", task.ID)
	if e != nil || q.(map[string]any)["patch_status"] != "PATCH_VERIFIED_REVIEW_REQUIRED" {
		t.Fatal("checked patch not labeled review required")
	}
	if m.state.CompositionConflicts[0].Status != "requires_resolution" || m.state.CompositionConflicts[0].ResolutionCommit != "" {
		t.Fatal("model/check report silently resolved conflict")
	}
	// The lease report is authenticated executor evidence, not an independently
	// verified Git source commit, CI publication, merge or deployment.
}

func TestIntelligencePatchStaleScopeAndRecipeGuards(t *testing.T) {
	t.Run("foreign participant", func(t *testing.T) {
		s, _, _, _, _ := patchTaskFixture(t)
		exec(t, s, domain.Command{Action: "create_feature", ID: "other-feature", ProductID: "p", Data: map[string]any{"title": "Other", "goal": "Other"}})
		c := domain.Command{Action: "queue_intelligence_task", Actor: "agent", ProductID: "p", FeatureID: "other-feature", Data: map[string]any{"task_kind": "conflict_patch", "request_key": "foreign-feature-1", "conflict_id": "conflict", "head_sha": strings.Repeat("b", 40), "files": []intelligenceFile{{Path: "src/value.go", Base: "base", Ours: "ours", Theirs: "theirs"}}}}
		reject(t, s, c)
	})
	t.Run("changed conflict before claim", func(t *testing.T) {
		s, m, _, _, c := patchTaskFixture(t)
		j := exec(t, s, c).(domain.WorkerJob)
		m.state.CompositionConflicts[0].Source.Plan.HeadSHAs = []string{strings.Repeat("d", 40)}
		p, _ := s.externalWorkers.Authenticate(workerTestToken, time.Now())
		lease, e := s.ClaimWorker(context.Background(), p, domain.WorkerClaim{ProductID: "p", RepositoryID: "repo", Capability: "source_check"})
		if e != nil || lease != nil || workerJob(&m.state, j.ID).Status != "RECONCILIATION_REQUIRED" {
			t.Fatal("stale conflict patch claimed", e)
		}
	})
	t.Run("revoked authority before queue", func(t *testing.T) {
		s, _, _, _, c := patchTaskFixture(t)
		s.ConfigureIntelligence(nil)
		reject(t, s, c)
	})
	t.Run("cancellation after grant removal", func(t *testing.T) {
		s, m, _, task, _ := patchTaskFixture(t)
		operationByID(&m.state, task.ID).Status = "PENDING"
		s.ConfigureIntelligence(nil)
		if _, e := s.Execute(context.Background(), domain.Command{Action: "cancel_intelligence_task", ProductID: "p", Actor: "operator", ID: task.ID, Data: map[string]any{}}); e != nil || operationByID(&m.state, task.ID).Status != "CANCELLED" {
			t.Fatal("cannot cancel task after provider removal", e)
		}
	})
}
