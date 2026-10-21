package application

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/workerconfig"
)

const workerTestToken = "fixture-worker-token-000000000000000000001"

func workerAuthority(t *testing.T) *workerconfig.Authority {
	return workerAuthorityRevision(t, "cfg1")
}
func workerAuthorityRevision(t *testing.T, revision string) *workerconfig.Authority {
	t.Helper()
	r := workerconfig.Recipe{ID: "check", Revision: "v1", ProductID: "p", RepositoryID: "r", RepositoryURL: "https://github.com/test/repo", Capability: "source_check", Trust: "trusted_source", Parameters: map[string]workerconfig.Parameter{"race": {Type: "boolean"}}}
	r.Hash = workerconfig.RecipeHash(r)
	a, e := workerconfig.New(workerconfig.Config{Revision: revision, LeaseSeconds: 5, Recipes: []workerconfig.Recipe{r}, Credentials: []workerconfig.Credential{{ID: "runner1", Principal: "worker/one", TokenEnv: "TEST_WORKER_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"r"}, Capabilities: []string{"source_check"}, ExpiresAt: time.Now().Add(time.Hour)}}}, "fixture-control-token", func(string) string { return workerTestToken })
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func workerSeed(t *testing.T, s *Service) {
	t.Helper()
	for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "p"}}, {Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "f", "goal": "g"}}, {Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "i", "objective": "o"}}, {Action: "create_repository", ID: "r", ProductID: "p", Data: map[string]any{"name": "repo", "url": "https://github.com/test/repo"}}, {Action: "record_integration_revision", ID: "rev", IntegrationID: "i", Data: map[string]any{"repository_id": "r", "branch": "feature", "base_commit": strings.Repeat("a", 40), "head_commit": strings.Repeat("b", 40), "commits": []string{strings.Repeat("b", 40)}}}} {
		c.Actor = "operator"
		if _, e := s.Execute(context.Background(), c); e != nil {
			t.Fatal(e)
		}
	}
}
func queueWorker(t *testing.T, s *Service) domain.WorkerJob {
	t.Helper()
	v, e := s.Execute(context.Background(), queueWorkerCommand(s))
	if e != nil {
		t.Fatal(e)
	}
	return v.(domain.WorkerJob)
}
func queueWorkerCommand(s *Service) domain.Command {
	r, _ := s.externalWorkers.Recipe("check")
	return domain.Command{Action: "queue_worker_job", Actor: "human/operator", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": id(), "recipe_id": r.ID, "recipe_revision": r.Revision, "recipe_hash": r.Hash, "config_revision": s.externalWorkers.Revision(), "revision_id": "rev", "source_sha": strings.Repeat("b", 40), "parameters": map[string]any{"race": true}, "deadline": time.Now().Add(time.Minute), "max_attempts": 2}}
}
func workerIdentity(t *testing.T, s *Service) workerconfig.Principal {
	t.Helper()
	p, e := s.externalWorkers.Authenticate(workerTestToken, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func claimWorker(t *testing.T, s *Service) *domain.WorkerLease {
	t.Helper()
	v, e := s.ClaimWorker(context.Background(), workerIdentity(t, s), domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "source_check"})
	if e != nil || v == nil {
		t.Fatal("claim", e)
	}
	return v
}
func workerRequest(v *domain.WorkerLease) domain.WorkerAttemptRequest {
	return domain.WorkerAttemptRequest{JobID: v.Job.ID, AttemptID: v.Attempt.ID, Fence: v.Attempt.Fence, InputHash: v.Job.InputHash}
}
func expireWorker(t *testing.T, store Store, attempt string) {
	t.Helper()
	if e := store.Update(context.Background(), func(st *domain.State) error {
		workerAttempt(st, attempt).LeaseUntil = time.Now().Add(-time.Second)
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

type workerTestStore interface {
	Store
	Close()
}

func openWorkerTestStore(t *testing.T, kind string) (workerTestStore, func() workerTestStore) {
	t.Helper()
	ctx := context.Background()
	if kind == "local" {
		path := filepath.Join(t.TempDir(), "worker.db")
		open := func() workerTestStore {
			s, e := persistence.OpenLocal(ctx, path)
			if e != nil {
				t.Fatal(e)
			}
			return s
		}
		return open(), open
	}
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL unset; PostgreSQL not verified")
	}
	conn, e := pgx.Connect(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	schema := fmt.Sprintf("worker_%d", time.Now().UnixNano())
	if _, e = conn.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = conn.Close(context.Background())
	})
	u, e := url.Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	open := func() workerTestStore {
		s, e := persistence.Open(ctx, u.String())
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	return open(), open
}
func TestWorkerDurabilityAndConcurrentClaims(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			store, reopen := openWorkerTestStore(t, kind)
			defer func() { store.Close() }()
			s := New(store)
			s.ConfigureWorkers(workerAuthority(t))
			workerSeed(t, s)
			job := queueWorker(t, s)
			p := workerIdentity(t, s)
			var wg sync.WaitGroup
			var mu sync.Mutex
			var claims []*domain.WorkerLease
			for range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					v, e := s.ClaimWorker(context.Background(), p, domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "source_check"})
					if e != nil {
						t.Error(e)
					}
					if v != nil {
						mu.Lock()
						claims = append(claims, v)
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
			if len(claims) != 1 {
				t.Fatalf("owners=%d", len(claims))
			}
			req := workerRequest(claims[0])
			before, e := store.Read(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if len(before.WorkerAttempts) != 1 || len(before.Operations) != 1 || before.WorkerJobs[0].OperationID != before.Operations[0].ID {
				t.Fatal("atomic claim records missing")
			}
			store.Close()
			store = reopen()
			s = New(store)
			s.ConfigureWorkers(workerAuthority(t))
			state, e := store.Read(context.Background())
			if e != nil || state.WorkerJobs[0].ID != job.ID || state.WorkerAttempts[0].Fence != 1 {
				t.Fatal("restart lost claim", e)
			}
			if _, e = s.StartWorker(context.Background(), p, req); e != nil {
				t.Fatal(e)
			}
			report := domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED", EvidenceHash: "sha256:" + strings.Repeat("c", 64)}
			if _, e = s.ReportWorker(context.Background(), p, report); e != nil {
				t.Fatal(e)
			}
			state, _ = store.Read(context.Background())
			events := len(state.Events)
			if _, e = s.ReportWorker(context.Background(), p, report); e != nil {
				t.Fatal("lost response replay", e)
			}
			state, _ = store.Read(context.Background())
			if len(state.Events) != events {
				t.Fatal("duplicate receipt mutated audit")
			}
			report.Result = "CHECK_FAILED"
			if _, e = s.ReportWorker(context.Background(), p, report); e == nil {
				t.Fatal("conflicting replay accepted")
			}
			if state.WorkerJobs[0].Status != "SUCCEEDED" || state.Operations[0].Status != "SUCCEEDED" {
				t.Fatal("operation lifecycle diverged")
			}
			// Store transactions must publish no partial audit/job/attempt mutation.
			if e = store.Update(context.Background(), func(st *domain.State) error {
				st.WorkerJobs[0].Fence = 99
				st.Events = append(st.Events, domain.Event{ID: "bad"})
				return errors.New("injected transaction failure")
			}); e == nil {
				t.Fatal("injection failed")
			}
			after, _ := store.Read(context.Background())
			if after.WorkerJobs[0].Fence != 1 || len(after.Events) != events {
				t.Fatal("partial transaction committed")
			}
		})
	}
}
func TestWorkerExpiryFencesCancellationAndRecovery(t *testing.T) {
	s, m := fixture(t)
	s.ConfigureWorkers(workerAuthority(t)) // fixture already owns p/f/i
	exec(t, s, domain.Command{Action: "create_repository", ID: "r", ProductID: "p", Data: map[string]any{"name": "repo", "url": "https://github.com/test/repo"}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "rev", IntegrationID: "i", Data: map[string]any{"repository_id": "r", "branch": "feature", "base_commit": strings.Repeat("a", 40), "head_commit": strings.Repeat("b", 40), "commits": []string{strings.Repeat("b", 40)}}})
	j := queueWorker(t, s)
	first := claimWorker(t, s)
	p := workerIdentity(t, s)
	old := workerRequest(first)
	expireWorker(t, m, first.Attempt.ID)
	if _, e := s.HeartbeatWorker(context.Background(), p, old); e == nil {
		t.Fatal("expired heartbeat accepted before reclaim")
	}
	if _, e := s.ReportWorker(context.Background(), p, domain.WorkerReport{WorkerAttemptRequest: old, Result: "PASSED"}); e == nil {
		t.Fatal("expired report accepted")
	}
	second := claimWorker(t, s)
	if second.Attempt.Fence != 2 {
		t.Fatal("nonmonotonic fence")
	}
	if _, e := s.StartWorker(context.Background(), p, old); e == nil {
		t.Fatal("stale fence accepted")
	}
	req := workerRequest(second)
	if _, e := s.StartWorker(context.Background(), p, req); e != nil {
		t.Fatal(e)
	}
	expireWorker(t, m, second.Attempt.ID)
	if _, e := s.HeartbeatWorker(context.Background(), p, req); e == nil {
		t.Fatal("running expiry accepted")
	}
	if workerJob(&m.state, j.ID).Status != "RECONCILIATION_REQUIRED" {
		t.Fatal("uncertain execution retried")
	}
	if v, e := s.ClaimWorker(context.Background(), p, domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "source_check"}); e != nil || v != nil {
		t.Fatal("uncertain work automatically reclaimed")
	}
	exec(t, s, domain.Command{Action: "reconcile_worker_job", ProductID: "p", ID: j.ID, Data: map[string]any{"outcome": "FAILED"}})
	j = queueWorker(t, s)
	v := claimWorker(t, s)
	exec(t, s, domain.Command{Action: "cancel_worker_job", ProductID: "p", ID: j.ID})
	if _, e := s.StartWorker(context.Background(), p, workerRequest(v)); e == nil {
		t.Fatal("cancelled claim started")
	}
	j = queueWorker(t, s)
	v = claimWorker(t, s)
	req = workerRequest(v)
	_, _ = s.StartWorker(context.Background(), p, req)
	exec(t, s, domain.Command{Action: "cancel_worker_job", ProductID: "p", ID: j.ID})
	if _, e := s.ReportWorker(context.Background(), p, domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED"}); e == nil {
		t.Fatal("cancel race accepted completion")
	}
	if workerJob(&m.state, j.ID).Status != "RECONCILIATION_REQUIRED" {
		t.Fatal("running cancellation lost uncertainty")
	}
}
func TestWorkerRejectsScopeAndImmutableInputMismatch(t *testing.T) {
	s, m := fixture(t)
	s.ConfigureWorkers(workerAuthority(t))
	exec(t, s, domain.Command{Action: "create_repository", ID: "r", ProductID: "p", Data: map[string]any{"name": "r", "url": "https://github.com/test/repo"}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "rev", IntegrationID: "i", Data: map[string]any{"repository_id": "r", "branch": "feature", "base_commit": strings.Repeat("a", 40), "head_commit": strings.Repeat("b", 40), "commits": []string{strings.Repeat("b", 40)}}})
	j := queueWorker(t, s)
	p := workerIdentity(t, s)
	for _, claim := range []domain.WorkerClaim{{ProductID: "other", RepositoryID: "r", Capability: "source_check"}, {ProductID: "p", RepositoryID: "other", Capability: "source_check"}, {ProductID: "p", RepositoryID: "r", Capability: "deploy"}} {
		if _, e := s.ClaimWorker(context.Background(), p, claim); e == nil {
			t.Fatal("scope escape")
		}
	}
	m.state.WorkerJobs[0].Inputs.SourceSHA = strings.Repeat("c", 40)
	if v, e := s.ClaimWorker(context.Background(), p, domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "source_check"}); e != nil || v != nil {
		t.Fatal("tampered source claimed")
	}
	if workerJob(&m.state, j.ID).Status != "RECONCILIATION_REQUIRED" {
		t.Fatal("tampering not quarantined")
	}
}
func TestWorkerDeadlineAttemptBudgetAndGitHubCoexistence(t *testing.T) {
	s, m, p := externalFixture(t)
	s.ConfigureWorkers(workerAuthority(t))
	m.state.WorkerJobs = append(m.state.WorkerJobs, domain.WorkerJob{Meta: domain.Meta{ID: "j", ProductID: "p"}, OperationID: "external", Capability: "source_check", Inputs: domain.WorkerInputs{RepositoryID: "r"}, Status: "QUEUED", Deadline: time.Now().Add(-time.Second), MaxAttempts: 1})
	m.state.Operations = append(m.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "external", ProductID: "p"}, Kind: domain.WorkerOperationKind, Status: "PENDING"})
	if _, e := s.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if m.state.Operations[0].Status != "PENDING" || p.dispatches != 0 {
		t.Fatal("GitHub poller claimed external operation")
	}
	_, e := s.ClaimWorker(context.Background(), workerIdentity(t, s), domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "source_check"})
	if e != nil {
		t.Fatal(e)
	}
	if m.state.WorkerJobs[0].Status != "DEADLINE_EXCEEDED" {
		t.Fatal("deadline ignored")
	}
}

func TestWorkerQueueIdempotencyConcurrentConflictAndReopen(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, reopen := openWorkerTestStore(t, kind)
			defer func() { store.Close() }()
			s := New(store)
			s.ConfigureWorkers(workerAuthority(t))
			workerSeed(t, s)
			command := queueWorkerCommand(s)
			deadline := time.Now().Add(200 * time.Millisecond)
			command.Data["deadline"] = deadline
			v, err := s.Execute(ctx, command)
			if err != nil {
				t.Fatal(err)
			}
			original := v.(domain.WorkerJob)
			var wg sync.WaitGroup
			for range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					v, e := s.Execute(ctx, command)
					if e != nil {
						t.Error(e)
						return
					}
					if v.(domain.WorkerJob).ID != original.ID {
						t.Error("duplicate execution created")
					}
				}()
			}
			wg.Wait()
			state, err := store.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			events := len(state.Events)
			if len(state.WorkerJobs) != 1 || len(state.Operations) != 1 || original.RequestHash == "" {
				t.Fatal("queue request not persisted atomically")
			}
			changed := command
			changed.Data = map[string]any{}
			for k, v := range command.Data {
				changed.Data[k] = v
			}
			changed.Data["parameters"] = map[string]any{"race": false}
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, e := s.Execute(ctx, changed); e == nil {
						t.Error("conflicting same-key request accepted")
					}
				}()
			}
			wg.Wait()
			store.Close()
			store = reopen()
			s = New(store)
			s.ConfigureWorkers(workerAuthorityRevision(t, "cfg2"))
			// Deadline passes and operator configuration changes while the caller
			// retries a lost queue response. It must still name the first job.
			if wait := time.Until(deadline) + time.Millisecond; wait > 0 {
				time.Sleep(wait)
			}
			v, err = s.Execute(ctx, command)
			if err != nil || v.(domain.WorkerJob).ID != original.ID {
				t.Fatal("lost response replay after restart/drift/expiry", err)
			}
			if _, err = s.Execute(ctx, changed); err == nil {
				t.Fatal("changed request accepted after restart")
			}
			state, err = store.Read(ctx)
			if err != nil || len(state.WorkerJobs) != 1 || len(state.Operations) != 1 || len(state.Events) != events {
				t.Fatal("retry produced work or audit", err)
			}
			// A deliberately new run gets a fresh key and current configuration.
			fresh := queueWorkerCommand(s)
			if _, err = s.Execute(ctx, fresh); err != nil {
				t.Fatal("new intentional run rejected", err)
			}
			state, _ = store.Read(ctx)
			if len(state.WorkerJobs) != 2 {
				t.Fatal("new request key did not create new work")
			}
		})
	}
}

func TestWorkerStartLostResponseReplay(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, _ := openWorkerTestStore(t, kind)
			defer store.Close()
			s := New(store)
			s.ConfigureWorkers(workerAuthority(t))
			workerSeed(t, s)
			job := queueWorker(t, s)
			lease := claimWorker(t, s)
			req := workerRequest(lease)
			p := workerIdentity(t, s)
			first, err := s.StartWorker(ctx, p, req)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := store.Read(ctx)
			second, err := s.StartWorker(ctx, p, req)
			after, _ := store.Read(ctx)
			if err != nil || second.Job.Status != "RUNNING" || second.Attempt.LeaseUntil != first.Attempt.LeaseUntil || len(before.Events) != len(after.Events) {
				t.Fatal("valid RUNNING replay changed receipt or audit", err)
			}
			if _, err = s.StartWorker(ctx, workerconfig.Principal{}, req); err == nil {
				t.Fatal("foreign replay accepted")
			}
			if _, err = s.Execute(ctx, domain.Command{Action: "cancel_worker_job", Actor: "human/operator", ProductID: "p", ID: job.ID}); err != nil {
				t.Fatal(err)
			}
			if _, err = s.StartWorker(ctx, p, req); err == nil {
				t.Fatal("cancelled RUNNING replay accepted")
			}
			queueWorker(t, s)
			lease = claimWorker(t, s)
			req = workerRequest(lease)
			if _, err = s.StartWorker(ctx, p, req); err != nil {
				t.Fatal(err)
			}
			expireWorker(t, store, lease.Attempt.ID)
			if _, err = s.StartWorker(ctx, p, req); err == nil {
				t.Fatal("expired RUNNING replay accepted")
			}
		})
	}
}

func TestWorkerBackupRestoreReconciliationAgreement(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			source, _ := openWorkerTestStore(t, kind)
			defer source.Close()
			s := New(source)
			s.ConfigureWorkers(workerAuthority(t))
			workerSeed(t, s)
			job := queueWorker(t, s)
			lease := claimWorker(t, s)
			req := workerRequest(lease)
			p := workerIdentity(t, s)
			if _, err := s.StartWorker(ctx, p, req); err != nil {
				t.Fatal(err)
			}
			queued := queueWorker(t, s)
			if err := source.Update(ctx, func(st *domain.State) error {
				st.Operations = append(st.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "github-pending", ProductID: "p", FeatureID: "f", Actor: "fixture"}, Kind: "REFRESH_GIT", Status: "PENDING"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			backup, err := s.CreateBackup(ctx, "human/operator")
			if err != nil {
				t.Fatal(err)
			}
			destination, _ := openWorkerTestStore(t, kind)
			defer destination.Close()
			restored := New(destination)
			restored.ConfigureWorkers(workerAuthority(t))
			response, err := restored.RestoreBackup(ctx, "human/operator", backup.ArchiveBase64, backup.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			result := response.(map[string]any)
			cancelled := result["cancelled_operation_ids"].([]string)
			reconciliation := result["reconciliation_required_operation_ids"].([]string)
			if len(cancelled) != 1 || cancelled[0] != "github-pending" || len(reconciliation) != 2 {
				t.Fatalf("restore response misrepresents execution: %+v", result)
			}
			state, err := destination.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, original := range []domain.WorkerJob{job, queued} {
				j := workerJob(&state, original.ID)
				op := operationByID(&state, original.OperationID)
				if j.Status != "RECONCILIATION_REQUIRED" || !j.CancellationRequested || op.Status != "RUNNING" || op.Phase != "EXTERNAL_RECONCILIATION_REQUIRED" || op.FinishedAt != nil || op.Error != "WORKER_RECONCILIATION_REQUIRED" || op.Detail == "" || op.LeaseOwner != "" || !op.LeaseUntil.IsZero() {
					t.Fatalf("restored job/operation disagree: %+v %+v", j, op)
				}
			}
			a := workerAttempt(&state, lease.Attempt.ID)
			if a.Status != "RECONCILIATION_REQUIRED" || a.LeaseUntil.After(time.Now()) {
				t.Fatal("restored attempt lease remained usable")
			}
			if _, err = restored.StartWorker(ctx, p, req); err == nil {
				t.Fatal("restored attempt restarted")
			}
			if _, err = restored.ReportWorker(ctx, p, domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED"}); err == nil {
				t.Fatal("restored attempt reported success")
			}
			if v, err := restored.ClaimWorker(ctx, p, domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "source_check"}); err != nil || v != nil {
				t.Fatal("restored work replayed", err)
			}
			for _, step := range state.OperationSteps {
				if step.OperationID == job.OperationID && step.Status == "CANCELLED" {
					t.Fatal("external operation incorrectly recorded cancelled restore step")
				}
			}
		})
	}
}

func TestWorkerLocalOCIAssertionBindingReplayAndReopen(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, reopen := openWorkerTestStore(t, kind)
			defer func() { store.Close() }()
			s := New(store)
			base := workerAuthority(t)
			recipe, _ := base.Recipe("check")
			recipe.Capability = "local_oci_build"
			recipe.Execution = &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: "golang@sha256:" + strings.Repeat("a", 64), BuildKitImage: "moby/buildkit@sha256:" + strings.Repeat("b", 64), Platform: "linux/arm64", CheckArgv: []string{"make", "check"}, BuildArgv: []string{"make", "build"}, OutputFile: "app", ImagePath: "app", Entrypoint: []string{"/app"}, Network: "none", TimeoutSeconds: 60, MemoryMB: 256, StorageMB: 64, CPUs: 1, PIDs: 64, SourceBytes: 1 << 20, OutputBytes: 4 << 20, LogBytes: 4096}
			recipe.Hash = workerconfig.RecipeHash(recipe)
			config := workerconfig.Config{Revision: "cfg1", LeaseSeconds: 20, Recipes: []workerconfig.Recipe{recipe}, Credentials: []workerconfig.Credential{{ID: "runner", Principal: "worker/one", TokenEnv: "TEST_WORKER_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"r"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}}}
			authority, err := workerconfig.New(config, "control", func(string) string { return workerTestToken })
			if err != nil {
				t.Fatal(err)
			}
			s.ConfigureWorkers(authority)
			workerSeed(t, s)
			j := queueWorker(t, s)
			p := workerIdentity(t, s)
			lease, err := s.ClaimWorker(ctx, p, domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "local_oci_build"})
			if err != nil || lease == nil {
				t.Fatal(err)
			}
			req := workerRequest(lease)
			if _, err = s.StartWorker(ctx, p, req); err != nil {
				t.Fatal(err)
			}
			good := domain.WorkerLocalOCI{JobID: j.ID, AttemptID: lease.Attempt.ID, InputHash: j.InputHash, SourceSHA: j.Inputs.SourceSHA, RecipeHash: j.Inputs.RecipeHash, ConfigRevision: j.Inputs.ConfigRevision, ToolchainImage: recipe.Execution.ToolchainImage, Platform: recipe.Execution.Platform, ArchiveHash: "sha256:" + strings.Repeat("c", 64), LayoutHash: "sha256:" + strings.Repeat("d", 64), IndexDigest: "sha256:" + strings.Repeat("e", 64), ManifestDigest: "sha256:" + strings.Repeat("f", 64), ArchiveBytes: 1234}
			for _, mutate := range []func(*domain.WorkerLocalOCI){func(v *domain.WorkerLocalOCI) { v.JobID = "foreign" }, func(v *domain.WorkerLocalOCI) { v.AttemptID = "foreign" }, func(v *domain.WorkerLocalOCI) { v.InputHash = "wrong" }, func(v *domain.WorkerLocalOCI) { v.SourceSHA = strings.Repeat("c", 40) }, func(v *domain.WorkerLocalOCI) { v.RecipeHash = "wrong" }, func(v *domain.WorkerLocalOCI) { v.ConfigRevision = "other" }, func(v *domain.WorkerLocalOCI) { v.ToolchainImage = "other" }, func(v *domain.WorkerLocalOCI) { v.Platform = "linux/amd64" }, func(v *domain.WorkerLocalOCI) { v.ArchiveBytes = 9 << 20 }, func(v *domain.WorkerLocalOCI) { v.ManifestDigest = "tag:latest" }} {
				bad := good
				mutate(&bad)
				if _, err = s.ReportWorker(ctx, p, domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED", LocalOCI: &bad}); err == nil {
					t.Fatal("mismatched local assertion accepted")
				}
			}
			if _, err = s.ReportWorker(ctx, p, domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED"}); err == nil {
				t.Fatal("build succeeded without local OCI output")
			}
			report := domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED", LocalOCI: &good}
			if _, err = s.ReportWorker(ctx, p, report); err != nil {
				t.Fatal(err)
			}
			store.Close()
			store = reopen()
			s = New(store)
			s.ConfigureWorkers(authority)
			before, _ := store.Read(ctx)
			if _, err = s.ReportWorker(ctx, p, report); err != nil {
				t.Fatal("identical typed result replay after reopen", err)
			}
			after, _ := store.Read(ctx)
			if len(before.Events) != len(after.Events) || after.WorkerAttempts[0].Report.LocalOCI.ManifestDigest != good.ManifestDigest || len(after.DeliveryArtifacts) != 0 || len(after.DeliveryBuildRuns) != 0 {
				t.Fatal("local assertion lost or misrepresented as publication")
			}
			changed := good
			changed.ManifestDigest = "sha256:" + strings.Repeat("0", 64)
			report.LocalOCI = &changed
			if _, err = s.ReportWorker(ctx, p, report); err == nil {
				t.Fatal("conflicting typed receipt replay accepted")
			}
		})
	}
}
