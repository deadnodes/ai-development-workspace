package application

import (
	"context"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

type fakeRuntimeObserver struct {
	calls          int
	target         delivery.RuntimeTarget
	evidence       delivery.RuntimeEvidence
	commit, digest string
}

func (f *fakeRuntimeObserver) TargetIdentity() delivery.RuntimeTarget { return f.target }
func (f *fakeRuntimeObserver) Observe(_ context.Context, commit, digest string) delivery.RuntimeEvidence {
	f.calls++
	f.commit = commit
	f.digest = digest
	return f.evidence
}
func TestRuntimeObserverScopesAndPersistsEvidence(t *testing.T) {
	m := &memoryStore{state: domain.EmptyState()}
	// The observer must never certify another Product's newer deployment.
	now := time.Now().UTC()
	m.state.Operations = []domain.ExternalOperation{
		{Meta: domain.Meta{ID: "own", ProductID: "p", CreatedAt: now}, EnvironmentID: "env", ApplicationID: "app", Status: "SUCCEEDED", GitOpsResult: &delivery.GitOpsResult{CommitSHA: "own-commit"}, Artifact: &delivery.ArtifactResult{Digest: "own-digest"}},
		{Meta: domain.Meta{ID: "other", ProductID: "other", CreatedAt: now.Add(time.Hour)}, EnvironmentID: "env", ApplicationID: "app", Status: "SUCCEEDED", GitOpsResult: &delivery.GitOpsResult{CommitSHA: "other-commit"}, Artifact: &delivery.ArtifactResult{Digest: "other-digest"}},
	}
	s := New(m)
	o := &fakeRuntimeObserver{target: delivery.RuntimeTarget{ProductID: "p", EnvironmentID: "env", ApplicationID: "app"}, evidence: delivery.RuntimeEvidence{Details: `{"runtime":"UNKNOWN"}`}}
	if e := s.ObserveRuntimeOnce(context.Background(), []delivery.RuntimeObserver{o}); e != nil {
		t.Fatal(e)
	}
	if o.commit != "own-commit" || o.digest != "own-digest" || len(m.state.RuntimeObservations) != 1 || m.state.RuntimeObservations[0].Healthy {
		t.Fatalf("wrong observation %+v", m.state.RuntimeObservations)
	}
	if e := s.ObserveRuntimeOnce(context.Background(), []delivery.RuntimeObserver{o}); e != nil {
		t.Fatal(e)
	}
	if len(m.state.RuntimeObservations) != 1 {
		t.Fatal("duplicate unchanged poll")
	}
	o.evidence = delivery.RuntimeEvidence{Healthy: true, Details: "exact healthy observation"}
	if e := s.ObserveRuntimeOnce(context.Background(), []delivery.RuntimeObserver{o}); e != nil {
		t.Fatal(e)
	}
	if len(m.state.RuntimeObservations) != 2 || !m.state.RuntimeObservations[1].Healthy {
		t.Fatal("changed evidence missing")
	}
}

func TestObservedDeploymentEnvironmentAndAttention(t *testing.T) {
	s, m, _ := flowFixture(t)
	op := domain.ExternalOperation{Meta: domain.Meta{ID: "direct", ProductID: "p"}, Kind: "DEPLOY", EnvironmentID: "dev", Status: "SUCCEEDED", DeploymentState: "GITOPS_APPLIED", GitOpsResult: &delivery.GitOpsResult{CommitSHA: "commit"}, Artifact: &delivery.ArtifactResult{Digest: "digest"}}
	m.state.Operations = append(m.state.Operations, op)
	m.state.RuntimeObservations = append(m.state.RuntimeObservations, domain.RuntimeObservation{Meta: domain.Meta{ProductID: "p", Actor: "system/runtime-observer"}, OperationID: op.ID, EnvironmentID: "dev", GitOpsCommit: "commit", ArtifactDigest: "digest", Healthy: true, Details: "server-collected evidence"})
	value, e := s.Query(context.Background(), "get_environment_state", "dev")
	if e != nil {
		t.Fatal(e)
	}
	result := value.(map[string]any)
	if strings.Contains(result["reconciliation"].(string), "no direct Flux observer") || len(result["runtime_observations"].([]domain.RuntimeObservation)) != 1 {
		t.Fatal("observer evidence misrepresented")
	}
	value, e = s.Query(context.Background(), "list_attention", "p")
	if e != nil {
		t.Fatal(e)
	}
	for _, item := range value.([]map[string]any) {
		if item["id"] == op.ID {
			t.Fatal("healthy exact observation remains pending", item)
		}
	}
	m.state.RuntimeObservations[0].Healthy = false
	value, e = s.Query(context.Background(), "list_attention", "p")
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, item := range value.([]map[string]any) {
		if item["id"] == op.ID && item["reason"] == "PENDING_RECONCILIATION" {
			found = true
		}
	}
	if !found {
		t.Fatal("unhealthy observation must retain pending attention")
	}
}

// Read fails through borrowedQueryStore, so this also guards against full clones.
type runtimeViewStore struct {
	borrowedQueryStore
	inView bool
	views  int
}

func (s *runtimeViewStore) View(ctx context.Context, fn func(*domain.State) error) error {
	s.views++
	s.inView = true
	defer func() { s.inView = false }()
	return fn(&s.state)
}

type unlockedRuntimeObserver struct {
	fakeRuntimeObserver
	store *runtimeViewStore
	t     *testing.T
}

func (o *unlockedRuntimeObserver) Observe(ctx context.Context, commit, digest string) delivery.RuntimeEvidence {
	if o.store.inView {
		o.t.Fatal("observer I/O inside store view")
	}
	// A write must also be possible while the provider is running.
	if err := o.store.Update(ctx, func(*domain.State) error { return nil }); err != nil {
		o.t.Fatal(err)
	}
	return o.fakeRuntimeObserver.Observe(ctx, commit, digest)
}
func TestRuntimeObserverIndexedSelectionAndDedupe(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name    string
		age     time.Duration
		healthy bool
		details string
		want    int
	}{
		{"recent", 10 * time.Second, true, "same", 0},
		{"expired", 2 * time.Minute, true, "same", 1},
		{"health changed", 10 * time.Second, false, "same", 1},
		{"details changed", 10 * time.Second, true, "different", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &runtimeViewStore{borrowedQueryStore: borrowedQueryStore{state: domain.EmptyState()}}
			target := delivery.RuntimeTarget{ProductID: "p", EnvironmentID: "env", ApplicationID: "app"}
			makeOp := func(id string, at time.Time) domain.ExternalOperation {
				return domain.ExternalOperation{Meta: domain.Meta{ID: id, ProductID: "p", CreatedAt: at}, Kind: "DEPLOY", EnvironmentID: "env", ApplicationID: "app", Status: "SUCCEEDED", GitOpsResult: &delivery.GitOpsResult{CommitSHA: id}, Artifact: &delivery.ArtifactResult{Digest: id + "-digest"}}
			}
			latest := makeOp("latest", now)
			other := makeOp("other", now.Add(time.Hour))
			other.ProductID = "other"
			env := makeOp("env", now.Add(time.Hour))
			env.EnvironmentID = "other"
			app := makeOp("app", now.Add(time.Hour))
			app.ApplicationID = "other"
			failed := makeOp("failed", now.Add(time.Hour))
			failed.Status = "FAILED"
			missing := makeOp("missing", now.Add(time.Hour))
			missing.Artifact = nil
			noGitOps := makeOp("no-gitops", now.Add(time.Hour))
			noGitOps.GitOpsResult = nil
			store.state.Operations = []domain.ExternalOperation{makeOp("old", now.Add(-time.Hour)), latest, other, env, app, failed, missing, noGitOps, makeOp("tie", now)}
			store.state.RuntimeObservations = []domain.RuntimeObservation{
				{Meta: domain.Meta{CreatedAt: now.Add(-tc.age)}, OperationID: "latest", Healthy: tc.healthy, Details: tc.details},
				{Meta: domain.Meta{CreatedAt: now.Add(-time.Hour)}, OperationID: "latest", Healthy: false, Details: "older"},
				{Meta: domain.Meta{CreatedAt: now.Add(time.Hour)}, OperationID: "old", Healthy: false, Details: "unrelated"},
			}
			observer := &unlockedRuntimeObserver{fakeRuntimeObserver: fakeRuntimeObserver{target: target, evidence: delivery.RuntimeEvidence{Healthy: true, Details: "same"}}, store: store, t: t}
			unknown := &fakeRuntimeObserver{target: delivery.RuntimeTarget{ProductID: "unknown"}}
			if err := New(store).ObserveRuntimeOnce(context.Background(), []delivery.RuntimeObserver{observer, unknown}); err != nil {
				t.Fatal(err)
			}
			if store.views != 1 || observer.calls != 1 || unknown.calls != 0 || observer.commit != "latest" || observer.digest != "latest-digest" {
				t.Fatalf("selection: views=%d observer=%+v unknown=%+v", store.views, observer, unknown)
			}
			if got := len(store.state.RuntimeObservations) - 3; got != tc.want {
				t.Fatalf("new observations=%d want %d", got, tc.want)
			}
			if tc.want == 1 {
				r := store.state.RuntimeObservations[3]
				if r.OperationID != "latest" || r.Actor != "system/runtime-observer" || !r.Healthy || len(store.state.Events) == 0 {
					t.Fatalf("command evidence/events missing: %+v", r)
				}
			}
		})
	}
}
