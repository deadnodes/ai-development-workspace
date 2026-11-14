package application

import (
	"context"
	"fmt"
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
	if strings.Contains(result["reconciliation"].(string), "no direct Flux observer") || len(result["runtime_observations"].([]runtimeObservationView)) != 1 {
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
		{"expired", 2 * time.Minute, true, "same", 0},
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
				{Meta: domain.Meta{CreatedAt: now.Add(-tc.age)}, OperationID: "latest", GitOpsCommit: "latest", ArtifactDigest: "latest-digest", Healthy: tc.healthy, Details: tc.details},
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

// Serialize the competing writer at the two race windows: while polling, and
// after the poll's command commits but before its freshness touch.
type racingFreshnessStore struct {
	memoryStore
	afterUpdate func()
	fresh       domain.RuntimeFreshness
}

func (m *racingFreshnessStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	if err := m.memoryStore.Update(ctx, fn); err != nil {
		return err
	}
	if m.afterUpdate != nil {
		m.afterUpdate()
	}
	return nil
}
func (m *racingFreshnessStore) TouchRuntimeFreshness(_ context.Context, _, observationID string, at time.Time, healthy bool) error {
	m.fresh = domain.RuntimeFreshness{ObservationID: observationID, ObservedAt: at, Healthy: healthy}
	return nil
}
func (m *racingFreshnessStore) RuntimeFreshness(context.Context, []string) (map[string]domain.RuntimeFreshness, error) {
	return map[string]domain.RuntimeFreshness{"op": m.fresh}, nil
}

type racingRuntimeObserver struct {
	fakeRuntimeObserver
	duringPoll func()
}

func (o *racingRuntimeObserver) Observe(ctx context.Context, commit, digest string) delivery.RuntimeEvidence {
	if o.duringPoll != nil {
		o.duringPoll()
	}
	return o.fakeRuntimeObserver.Observe(ctx, commit, digest)
}

func TestRuntimeObserverFreshnessRace(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed=%t", changed), func(t *testing.T) {
			now := time.Now().UTC()
			m := &racingFreshnessStore{memoryStore: memoryStore{state: domain.EmptyState()}}
			m.state.Operations = []domain.ExternalOperation{{Meta: domain.Meta{ID: "op", ProductID: "p", CreatedAt: now}, Status: "SUCCEEDED", EnvironmentID: "env", ApplicationID: "app", GitOpsResult: &delivery.GitOpsResult{CommitSHA: "commit"}, Artifact: &delivery.ArtifactResult{Digest: "digest"}}}
			previous := domain.RuntimeObservation{Meta: domain.Meta{ID: "previous", ProductID: "p", CreatedAt: now.Add(-time.Hour)}, OperationID: "op", EnvironmentID: "env", GitOpsCommit: "commit", ArtifactDigest: "digest", Healthy: true, Details: "same"}
			m.state.RuntimeObservations = []domain.RuntimeObservation{previous}
			wantID := previous.ID
			compete := func() {
				// Channel synchronization makes the interleaving deterministic and
				// keeps the fixture itself safe under the Go race detector.
				done := make(chan struct{})
				go func() {
					defer close(done)
					wantID = m.state.RuntimeObservations[len(m.state.RuntimeObservations)-1].ID
					other := previous
					other.ID, other.Details, other.CreatedAt = "concurrent", "different details", now.Add(time.Hour)
					m.state.RuntimeObservations = append(m.state.RuntimeObservations, other)
				}()
				<-done
			}
			o := &racingRuntimeObserver{fakeRuntimeObserver: fakeRuntimeObserver{target: delivery.RuntimeTarget{ProductID: "p", EnvironmentID: "env", ApplicationID: "app"}, evidence: delivery.RuntimeEvidence{Healthy: true, Details: "same"}}}
			if changed {
				o.evidence.Details = "changed by poll"
				m.afterUpdate = compete
			} else {
				o.duringPoll = compete
			}
			if err := New(m).ObserveRuntimeOnce(context.Background(), []delivery.RuntimeObserver{o}); err != nil {
				t.Fatal(err)
			}
			if m.fresh.ObservationID != wantID || m.fresh.ObservedAt.IsZero() {
				t.Fatalf("freshness attached to %q, want poll evidence %q", m.fresh.ObservationID, wantID)
			}
			if bound := domain.BindRuntimeFreshness(m.state.RuntimeObservations, map[string]domain.RuntimeFreshness{"op": m.fresh}); !bound["concurrent"].IsZero() {
				t.Fatal("concurrent evidence received this poll's freshness")
			}
		})
	}
}
