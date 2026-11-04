package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"releasecontrol/internal/persistence"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/intelligence"
)

// The model port is fake; crypto, grants, command admission, durable operations,
// fencing and scoped query all use the production application implementation.
type intelligenceFixtureProvider struct {
	*intelligence.Authority
	result string
	err    error
	calls  int
	onCall func()
}

func (f *intelligenceFixtureProvider) Complete(ctx context.Context, _ string, input any) (json.RawMessage, intelligence.Usage, error) {
	f.calls++
	if f.onCall != nil {
		f.onCall()
	}
	if e := f.ValidateInput(input); e != nil {
		return nil, intelligence.Usage{}, e
	}
	return json.RawMessage(f.result), intelligence.Usage{Reported: true, PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}, f.err
}
func intelligenceFixture(t *testing.T, max int) (*Service, *memoryStore, *intelligenceFixtureProvider) {
	t.Helper()
	s, m := fixture(t)
	a, e := intelligence.New(intelligence.Config{BaseURL: "https://model.example/v1", Model: "fake-model", APIKeyEnv: "MODEL_KEY", EncryptionKeyEnv: "CIPHER_KEY", ProductIDs: []string{"p"}, ConflictPaths: map[string][]string{"repo": {"src/value.go"}}, MaxInputBytes: 65536, MaxOutputBytes: 32768, MaxCompletionTokens: 1024, MaxRequestsPerDay: max, TimeoutSeconds: 2}, func(k string) string {
		if k == "MODEL_KEY" {
			return strings.Repeat("x", 32)
		}
		return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("y", 32)))
	})
	if e != nil {
		t.Fatal(e)
	}
	f := &intelligenceFixtureProvider{Authority: a, result: `{"decision":"draft","summary":"generated private wording","changes":[],"warnings":[],"patch":null}`}
	s.ConfigureIntelligence(f)
	return s, m, f
}
func intelligenceCommand(kind, key string) domain.Command {
	return domain.Command{Actor: "agent", ProductID: "p", FeatureID: "f", Action: "queue_intelligence_task", Data: map[string]any{"task_kind": kind, "request_key": key}}
}
func runIntelligenceTick(t *testing.T, s *Service) {
	t.Helper()
	ran, e := s.Tick(context.Background())
	if e != nil || !ran {
		t.Fatalf("tick ran=%v err=%v", ran, e)
	}
}

func TestIntelligenceDurableEncryptedScopedDraft(t *testing.T) {
	s, m, f := intelligenceFixture(t, 10)
	c := intelligenceCommand("summary", "request-unique-0001")
	c.Data["excerpts"] = []string{"private diagnostic alpha"}
	op := exec(t, s, c).(domain.ExternalOperation)
	first, _ := json.Marshal(m.state)
	if strings.Contains(string(first), "private diagnostic alpha") {
		t.Fatal("raw input leaked into state/audit")
	}
	// Reconstruct the service as after a process restart before the first call.
	s = New(m)
	s.ConfigureIntelligence(f)
	runIntelligenceTick(t, s)
	if f.calls != 1 || operationByID(&m.state, op.ID).Status != "SUCCEEDED" {
		t.Fatal("durable model task did not complete")
	}
	stored, _ := json.Marshal(m.state)
	if strings.Contains(string(stored), "private diagnostic alpha") || strings.Contains(string(stored), "generated private wording") {
		t.Fatal("plaintext model input/output leaked into records/events")
	}
	q, e := s.Query(context.Background(), "get_intelligence_task", op.ID)
	if e != nil {
		t.Fatal(e)
	}
	draft := q.(map[string]any)
	if draft["stale"] != false || draft["published"] != false || draft["review_required"] != true || draft["result"].(intelligenceResult).Summary != "generated private wording" {
		t.Fatal("missing draft semantics")
	}
	replayed := exec(t, s, c).(domain.ExternalOperation)
	if replayed.ID != op.ID || len(m.state.Operations) != 1 {
		t.Fatal("idempotence created a new task")
	}
	ran, _ := s.Tick(context.Background())
	if ran || f.calls != 1 {
		t.Fatal("terminal task called provider twice")
	}
	c.Data["excerpts"] = []string{"different"}
	reject(t, s, c)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/foreign", Kind: "agent", Permissions: []string{"read"}, Products: []string{"q"}})
	if _, e = s.Query(ctx, "get_intelligence_task", op.ID); e == nil {
		t.Fatal("cross-Product draft read admitted")
	}
}

func TestIntelligenceStaleCancelBudgetAndUncertainRecovery(t *testing.T) {
	t.Run("stale before dispatch", func(t *testing.T) {
		s, m, f := intelligenceFixture(t, 10)
		op := exec(t, s, intelligenceCommand("annotation", "request-stale-001")).(domain.ExternalOperation)
		m.state.Features[0].Goal = "new intent"
		runIntelligenceTick(t, s)
		if f.calls != 0 || operationByID(&m.state, op.ID).Status != "FAILED" {
			t.Fatal("stale context called provider")
		}
	})
	t.Run("late result after cancellation", func(t *testing.T) {
		s, m, f := intelligenceFixture(t, 10)
		op := exec(t, s, intelligenceCommand("annotation", "request-cancel-01")).(domain.ExternalOperation)
		f.onCall = func() {
			_, e := s.Execute(context.Background(), domain.Command{Action: "cancel_intelligence_task", Actor: "agent", ProductID: "p", ID: op.ID, Data: map[string]any{}})
			if e != nil {
				t.Error(e)
			}
		}
		_, e := s.Tick(context.Background())
		if e == nil || operationByID(&m.state, op.ID).Status != "CANCELLED" || operationByID(&m.state, op.ID).Intelligence.ResultCiphertext != "" {
			t.Fatal("late cancelled result admitted")
		}
	})
	t.Run("request budget", func(t *testing.T) {
		s, m, f := intelligenceFixture(t, 1)
		exec(t, s, intelligenceCommand("annotation", "request-budget-01"))
		exec(t, s, intelligenceCommand("annotation", "request-budget-02"))
		runIntelligenceTick(t, s)
		runIntelligenceTick(t, s)
		if f.calls != 1 {
			t.Fatalf("request budget bypass: %d", f.calls)
		}
		blocked := 0
		for _, v := range m.state.Operations {
			if v.Phase == "PROVIDER_REQUEST_BUDGET_EXHAUSTED" {
				blocked++
			}
		}
		if blocked != 1 {
			t.Fatal("budget state not actionable")
		}
	})
	t.Run("crash after dispatch intent", func(t *testing.T) {
		s, m, f := intelligenceFixture(t, 10)
		op := exec(t, s, intelligenceCommand("annotation", "request-crash-001")).(domain.ExternalOperation)
		v := operationByID(&m.state, op.ID)
		v.Status = "RUNNING"
		v.Phase = "DISPATCH_INTENT"
		v.Attempts = 1
		v.LeaseUntil = time.Now().Add(-time.Minute)
		s = New(m)
		s.ConfigureIntelligence(f)
		runIntelligenceTick(t, s)
		if f.calls != 0 || operationByID(&m.state, op.ID).Phase != "PROVIDER_OUTCOME_UNCERTAIN" {
			t.Fatal("uncertain paid request was replayed")
		}
	})
	t.Run("provider failure", func(t *testing.T) {
		s, m, f := intelligenceFixture(t, 10)
		f.err = errors.New("PROVIDER_OUTCOME_UNCERTAIN")
		op := exec(t, s, intelligenceCommand("annotation", "request-failure-1")).(domain.ExternalOperation)
		runIntelligenceTick(t, s)
		run, _ := s.Tick(context.Background())
		if run || f.calls != 1 || operationByID(&m.state, op.ID).Status != "FAILED" {
			t.Fatal("failure retried or lost")
		}
	})
	t.Run("privacy and cross scope", func(t *testing.T) {
		s, _, f := intelligenceFixture(t, 10)
		c := intelligenceCommand("log_summary", "request-privacy-1")
		c.Data["excerpts"] = []string{"password=someVerySecretPassword123"}
		reject(t, s, c)
		c = intelligenceCommand("annotation", "request-scope-001")
		c.ProductID = "foreign"
		reject(t, s, c)
		if f.calls != 0 {
			t.Fatal("rejected task contacted provider")
		}
	})
	t.Run("authority revocation", func(t *testing.T) {
		s, m, _ := intelligenceFixture(t, 10)
		op := exec(t, s, intelligenceCommand("annotation", "request-revoke-01")).(domain.ExternalOperation)
		s.ConfigureIntelligence(nil)
		if _, e := s.Query(context.Background(), "get_intelligence_task", op.ID); e != nil {
			t.Fatal(e)
		}
		if operationByID(&m.state, op.ID).Intelligence.ResultCiphertext != "" {
			t.Fatal("revoked authority emitted output")
		}
	})
}

func releaseNotesFixture(t *testing.T) (*Service, *memoryStore, *intelligenceFixtureProvider, domain.Command) {
	t.Helper()
	s, m, f := intelligenceFixture(t, 10)
	oldSHA, newSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	oldDigest, newDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	m.state.Environments = append(m.state.Environments, domain.Environment{Meta: domain.Meta{ID: "env", ProductID: "p"}, Name: "dev"})
	m.state.Integrations[0].Commits = []domain.Commit{{RepositoryID: "repo", SHA: oldSHA}}
	revision := domain.IntegrationRevision{Meta: domain.Meta{ID: "revision-new", ProductID: "p", FeatureID: "f"}, IntegrationID: "i", RepositoryID: "repo", HeadCommit: newSHA}
	comp := domain.Composition{Meta: domain.Meta{ID: "composition", ProductID: "p"}, RevisionSnapshots: []domain.IntegrationRevision{revision}}
	oldSnapshot := domain.DeliverySnapshot{Application: domain.Application{Meta: domain.Meta{ID: "app", ProductID: "p"}, RepositoryID: "repo"}, Revision: domain.IntegrationRevision{RepositoryID: "repo", HeadCommit: oldSHA}}
	newSnapshot := oldSnapshot
	newSnapshot.Revision.HeadCommit = newSHA
	m.state.Operations = append(m.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "old-deploy", ProductID: "p"}, Kind: "DEPLOY", ApplicationID: "app", EnvironmentID: "env", Snapshot: &oldSnapshot}, domain.ExternalOperation{Meta: domain.Meta{ID: "candidate", ProductID: "p"}, Kind: "RELEASE_CANDIDATE", EnvironmentID: "env", Status: "SUCCEEDED", CompositionSnapshot: &comp, ChildIDs: []string{"new-build"}, ReleaseNotesIntentHash: releaseNotesIntentHash(&m.state, comp)}, domain.ExternalOperation{Meta: domain.Meta{ID: "new-build", ProductID: "p"}, ParentID: "candidate", Kind: "DEPLOY", ApplicationID: "app", EnvironmentID: "env", Status: "SUCCEEDED", Snapshot: &newSnapshot, Artifact: &delivery.ArtifactResult{Digest: newDigest}})
	m.state.RuntimeObservations = append(m.state.RuntimeObservations, domain.RuntimeObservation{Meta: domain.Meta{ID: "runtime", ProductID: "p", CreatedAt: time.Now().Add(-time.Minute)}, OperationID: "old-deploy", EnvironmentID: "env", ArtifactDigest: oldDigest, GitOpsCommit: oldSHA, Healthy: true})
	m.state.Releases = append(m.state.Releases, domain.Release{Meta: domain.Meta{ID: "previous", ProductID: "p", CreatedAt: time.Now().Add(-time.Hour)}, EnvironmentID: "env", Status: "released", IntegrationIDs: []string{"i"}, SourceCommits: map[string]string{"app": oldSHA}, ArtifactDigests: map[string]string{"app": oldDigest}, GitOpsCommits: map[string]string{"app": oldSHA}, Snapshots: []domain.Integration{m.state.Integrations[0]}})
	f.result = `{"decision":"draft","summary":"Release summary","changes":[{"integration_ids":["i"],"text":"A useful improvement.","evidence_refs":["candidate","revision-new"]}],"warnings":[],"patch":null}`
	c := domain.Command{Actor: "agent", Action: "queue_intelligence_task", ProductID: "p", Data: map[string]any{"task_kind": "release_notes", "request_key": "release-notes-001", "candidate_operation_id": "candidate", "previous_release_id": "previous"}}
	return s, m, f, c
}

func TestReleaseNotesActualDeltaAndStaleCandidate(t *testing.T) {
	s, m, f, c := releaseNotesFixture(t)
	// Unrelated ready work exists but is not selected by the release candidate.
	m.state.Integrations = append(m.state.Integrations, domain.Integration{Meta: domain.Meta{ID: "unshipped", ProductID: "p", FeatureID: "f"}, Title: "Future feature", Status: "ready"})
	op := exec(t, s, c).(domain.ExternalOperation)
	runIntelligenceTick(t, s)
	q, e := s.Query(context.Background(), "get_intelligence_task", op.ID)
	if e != nil || q.(map[string]any)["stale"] != false || f.calls != 1 {
		t.Fatal("notes not generated")
	}
	provenance := q.(map[string]any)["provenance"].(notesContext)
	if len(provenance.Changes) != 1 || provenance.Changes[0].IntegrationID != "i" {
		t.Fatal("unselected work leaked into notes")
	}
	operationByID(&m.state, "candidate").RequestHash = "new candidate configuration"
	q, e = s.Query(context.Background(), "get_intelligence_task", op.ID)
	if e != nil || q.(map[string]any)["stale"] != true || q.(map[string]any)["result"] != nil {
		t.Fatal("stale notes still presented as current")
	}
}

func TestReleaseNotesNegativeBaselinesAndIntent(t *testing.T) {
	for name, change := range map[string]func(*memoryStore){
		"missing runtime": func(m *memoryStore) { m.state.RuntimeObservations = nil },
		"wrong current digest": func(m *memoryStore) {
			m.state.RuntimeObservations[0].ArtifactDigest = "sha256:" + strings.Repeat("c", 64)
		},
		"unhealthy current runtime": func(m *memoryStore) { m.state.RuntimeObservations[0].Healthy = false },
		"missing candidate capture": func(m *memoryStore) { operationByID(&m.state, "candidate").ReleaseNotesIntentHash = "" },
		"mutable future intent":     func(m *memoryStore) { m.state.Integrations[0].Objective = "Future unpublished behavior" },
		"missing revision baseline": func(m *memoryStore) { m.state.Releases[0].Snapshots[0].Commits = nil },
		"empty release delta": func(m *memoryStore) {
			operationByID(&m.state, "candidate").CompositionSnapshot.RevisionSnapshots[0].HeadCommit = strings.Repeat("a", 40)
		},
		"wrong environment": func(m *memoryStore) { m.state.Releases[0].EnvironmentID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			s, m, f, c := releaseNotesFixture(t)
			change(m)
			reject(t, s, c)
			if f.calls != 0 {
				t.Fatal("incomplete release contacted provider")
			}
		})
	}
}

func TestReleaseNotesUnchangedSameRepoAndNewFeature(t *testing.T) {
	s, m, _, c := releaseNotesFixture(t)
	// Integration i already shipped unchanged; new j advances the same repository.
	candidate := operationByID(&m.state, "candidate")
	candidate.CompositionSnapshot.RevisionSnapshots[0].HeadCommit = strings.Repeat("a", 40)
	m.state.Integrations = append(m.state.Integrations, domain.Integration{Meta: domain.Meta{ID: "j", ProductID: "p", FeatureID: "f"}, Title: "New behavior", Objective: "Ship new behavior", Status: "ready"})
	candidate = operationByID(&m.state, "candidate")
	candidate.CompositionSnapshot.RevisionSnapshots = append(candidate.CompositionSnapshot.RevisionSnapshots, domain.IntegrationRevision{Meta: domain.Meta{ID: "revision-j", ProductID: "p", FeatureID: "f"}, IntegrationID: "j", RepositoryID: "repo", HeadCommit: strings.Repeat("b", 40)})
	candidate.ReleaseNotesIntentHash = releaseNotesIntentHash(&m.state, *candidate.CompositionSnapshot)
	var request intelligenceRequest
	if e := decode(c.Data, &request); e != nil {
		t.Fatal(e)
	}
	ctx, e := s.releaseNotesContext(&m.state, "p", request)
	if e != nil || len(ctx.Changes) != 1 || ctx.Changes[0].IntegrationID != "j" {
		t.Fatalf("same-repo old feature mislabeled: %+v err %v", ctx.Changes, e)
	}
}

func TestReleaseNotesRejectUnshippedModelCitations(t *testing.T) {
	s, m, f, c := releaseNotesFixture(t)
	f.result = strings.Replace(f.result, `["i"]`, `["unshipped"]`, 1)
	op := exec(t, s, c).(domain.ExternalOperation)
	runIntelligenceTick(t, s)
	if operationByID(&m.state, op.ID).Status != "FAILED" || operationByID(&m.state, op.ID).Intelligence.ResultCiphertext != "" {
		t.Fatal("invented shipped feature admitted")
	}
}

func TestReleaseNotesDifferentComponentBaselinesAndHistoricalRecord(t *testing.T) {
	s, m, _, c := releaseNotesFixture(t)
	oldSHA, newSHA := strings.Repeat("c", 40), strings.Repeat("d", 40)
	oldDigest, newDigest := "sha256:"+strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64)
	m.state.Integrations = append(m.state.Integrations, domain.Integration{Meta: domain.Meta{ID: "other-integration", ProductID: "p", FeatureID: "f"}, Title: "Another service fix", Objective: "Fix another service", Commits: []domain.Commit{{RepositoryID: "other-repo", SHA: oldSHA}}})
	oldSnapshot := domain.DeliverySnapshot{Application: domain.Application{Meta: domain.Meta{ID: "other-app", ProductID: "p"}, RepositoryID: "other-repo"}, Revision: domain.IntegrationRevision{RepositoryID: "other-repo", HeadCommit: oldSHA}}
	newSnapshot := oldSnapshot
	newSnapshot.Revision.HeadCommit = newSHA
	m.state.Operations = append(m.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "other-deploy", ProductID: "p"}, Kind: "DEPLOY", ApplicationID: "other-app", EnvironmentID: "env", Snapshot: &oldSnapshot}, domain.ExternalOperation{Meta: domain.Meta{ID: "other-build", ProductID: "p"}, ParentID: "candidate", ApplicationID: "other-app", EnvironmentID: "env", Snapshot: &newSnapshot, Artifact: &delivery.ArtifactResult{Digest: newDigest}})
	m.state.RuntimeObservations = append(m.state.RuntimeObservations, domain.RuntimeObservation{Meta: domain.Meta{ID: "other-runtime", ProductID: "p", CreatedAt: time.Now()}, OperationID: "other-deploy", EnvironmentID: "env", ArtifactDigest: oldDigest, GitOpsCommit: oldSHA, Healthy: true})
	m.state.Releases = append(m.state.Releases, domain.Release{Meta: domain.Meta{ID: "other-previous", ProductID: "p"}, Status: "released", EnvironmentID: "env", IntegrationIDs: []string{"other-integration"}, SourceCommits: map[string]string{"other-app": oldSHA}, ArtifactDigests: map[string]string{"other-app": oldDigest}, GitOpsCommits: map[string]string{"other-app": oldSHA}, Snapshots: []domain.Integration{m.state.Integrations[len(m.state.Integrations)-1]}})
	candidate := operationByID(&m.state, "candidate")
	candidate.ChildIDs = append(candidate.ChildIDs, "other-build")
	candidate.CompositionSnapshot.RevisionSnapshots = append(candidate.CompositionSnapshot.RevisionSnapshots, domain.IntegrationRevision{Meta: domain.Meta{ID: "other-revision", ProductID: "p", FeatureID: "f"}, IntegrationID: "other-integration", RepositoryID: "other-repo", HeadCommit: newSHA})
	candidate.ReleaseNotesIntentHash = releaseNotesIntentHash(&m.state, *candidate.CompositionSnapshot)
	delete(c.Data, "previous_release_id")
	c.Data["previous_release_ids"] = map[string]string{"app": "previous", "other-app": "other-previous"}
	// A late historical release does not replace the actual per-component baseline.
	historical := m.state.Releases[0]
	historical.ID = "late-history"
	historical.CreatedAt = time.Now().Add(time.Hour)
	historical.ArtifactDigests = map[string]string{"app": "sha256:" + strings.Repeat("e", 64)}
	m.state.Releases = append(m.state.Releases, historical)
	var request intelligenceRequest
	if e := decode(c.Data, &request); e != nil {
		t.Fatal(e)
	}
	ctx, e := s.releaseNotesContext(&m.state, "p", request)
	if e != nil || len(ctx.Changes) != 2 || ctx.PreviousReleaseIDs["app"] != "previous" || ctx.PreviousReleaseIDs["other-app"] != "other-previous" {
		t.Fatalf("multi-component baseline failed: %+v %v", ctx, e)
	}
	request.PreviousReleaseIDs["app"] = "late-history"
	if _, e = s.releaseNotesContext(&m.state, "p", request); e == nil {
		t.Fatal("late historical record substituted for current runtime")
	}
}

func TestIntelligenceDecodedProviderPrivacy(t *testing.T) {
	for _, raw := range []string{`{"decision":"draft","summary":"person\u0040example.com","changes":[],"warnings":[],"patch":null}`, `{"decision":"draft","summary":"\u0078xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx","changes":[],"warnings":[],"patch":null}`} {
		s, m, f := intelligenceFixture(t, 10)
		f.result = raw
		op := exec(t, s, intelligenceCommand("annotation", "request-escaped-1")).(domain.ExternalOperation)
		runIntelligenceTick(t, s)
		current := operationByID(&m.state, op.ID)
		if current.Status != "FAILED" || current.Intelligence.ResultCiphertext != "" {
			t.Fatal("escaped provider output reached persistence")
		}
	}
	// Even historical authenticated ciphertext is revalidated before display.
	s, m, f := intelligenceFixture(t, 10)
	op := exec(t, s, intelligenceCommand("annotation", "request-query-01")).(domain.ExternalOperation)
	runIntelligenceTick(t, s)
	bad := intelligenceResult{Decision: "draft", Summary: "person@example.com"}
	current := operationByID(&m.state, op.ID)
	sealed, e := f.Seal(intelligenceAAD(*current, "result"), bad)
	if e != nil {
		t.Fatal(e)
	}
	current.Intelligence.ResultCiphertext = sealed
	current.Intelligence.ResultFingerprint = f.Fingerprint(bad)
	if _, e = s.Query(context.Background(), "get_intelligence_task", op.ID); e == nil {
		t.Fatal("historical decoded PII displayed")
	}
}

func TestIntelligenceFailureStatusesPersistBackupAndRestore(t *testing.T) {
	for _, reason := range []string{"stale", "uncertain", "budget"} {
		t.Run(reason, func(t *testing.T) {
			_, seed, f := intelligenceFixture(t, map[string]int{"budget": 1, "stale": 10, "uncertain": 10}[reason])
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.db")
			store, e := persistence.OpenLocal(ctx, path)
			if e != nil {
				t.Fatal(e)
			}
			if e = store.Update(ctx, func(st *domain.State) error { *st = seed.state; return nil }); e != nil {
				t.Fatal(e)
			}
			s := New(store)
			s.ConfigureIntelligence(f)
			task := exec(t, s, intelligenceCommand("annotation", "request-status-01")).(domain.ExternalOperation)
			switch reason {
			case "stale":
				exec(t, s, domain.Command{Action: "update_feature", FeatureID: "f", Data: map[string]any{"goal": "Changed intent"}})
			case "uncertain":
				if e = store.Update(ctx, func(st *domain.State) error {
					op := operationByID(st, task.ID)
					op.Status = "RUNNING"
					op.Phase = "DISPATCH_INTENT"
					op.LeaseUntil = time.Now().Add(-time.Minute)
					return nil
				}); e != nil {
					t.Fatal(e)
				}
			case "budget":
				exec(t, s, intelligenceCommand("annotation", "request-status-02"))
				runIntelligenceTick(t, s)
			}
			runIntelligenceTick(t, s)
			state, e := store.Read(ctx)
			if e != nil || domain.ValidateStateStatuses(state) != nil {
				t.Fatal("failure state violates controlled statuses", e)
			}
			failed := false
			for _, op := range state.Operations {
				if op.Status == "FAILED" {
					failed = true
				}
			}
			if !failed {
				t.Fatal("expected actionable terminal failure")
			}
			backup, e := s.CreateBackup(ctx, "operator")
			if e != nil {
				t.Fatal(e)
			}
			store.Close()
			store, e = persistence.OpenLocal(ctx, path)
			if e != nil {
				t.Fatal(e)
			}
			defer store.Close()
			state, e = store.Read(ctx)
			if e != nil || domain.ValidateStateStatuses(state) != nil {
				t.Fatal("persisted failure state cannot reopen", e)
			}
			destination, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "restored.db"))
			if e != nil {
				t.Fatal(e)
			}
			defer destination.Close()
			if _, e = New(destination).RestoreBackup(ctx, "operator", backup.ArchiveBase64, backup.SHA256); e != nil {
				t.Fatal("failure task backup cannot restore", e)
			}
		})
	}
}
