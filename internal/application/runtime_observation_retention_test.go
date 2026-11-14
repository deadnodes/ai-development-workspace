package application

import (
	"context"
	"fmt"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"testing"
	"time"
)

// A memory test store implements the same optional liveness contract without State.
type freshnessMemoryStore struct {
	*workerScopedCountingStore
	fresh   map[string]domain.RuntimeFreshness
	touches int
}

func (m *freshnessMemoryStore) TouchRuntimeFreshness(_ context.Context, id string, observationID string, at time.Time, healthy bool) error {
	if m.fresh == nil {
		m.fresh = map[string]domain.RuntimeFreshness{}
	}
	m.fresh[id] = domain.RuntimeFreshness{ObservationID: observationID, ObservedAt: at, Healthy: healthy}
	m.touches++
	return nil
}
func (m *freshnessMemoryStore) RuntimeFreshness(_ context.Context, ids []string) (map[string]domain.RuntimeFreshness, error) {
	out := map[string]domain.RuntimeFreshness{}
	for _, id := range ids {
		if at, ok := m.fresh[id]; ok {
			out[id] = at
		}
	}
	return out, nil
}
func TestRuntimeChangeOnlyTouchesFreshness(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	m := &freshnessMemoryStore{workerScopedCountingStore: &workerScopedCountingStore{workerCountingStore: &workerCountingStore{borrowedQueryStore: borrowedQueryStore{state: domain.EmptyState()}}}}
	m.state.Operations = []domain.ExternalOperation{{Meta: domain.Meta{ID: "op", ProductID: "p", CreatedAt: now}, Status: "SUCCEEDED", EnvironmentID: "env", ApplicationID: "app", GitOpsResult: &delivery.GitOpsResult{CommitSHA: "commit"}, Artifact: &delivery.ArtifactResult{Digest: "digest"}}}
	m.state.RuntimeObservations = []domain.RuntimeObservation{{Meta: domain.Meta{ID: "old", ProductID: "p", CreatedAt: now.Add(-48 * time.Hour)}, OperationID: "op", EnvironmentID: "env", GitOpsCommit: "commit", ArtifactDigest: "digest", Healthy: true, Details: "same"}}
	o := &fakeRuntimeObserver{target: delivery.RuntimeTarget{ProductID: "p", EnvironmentID: "env", ApplicationID: "app"}, evidence: delivery.RuntimeEvidence{Healthy: true, Details: "same"}}
	s := New(m)
	if e := s.ObserveRuntimeOnce(ctx, []delivery.RuntimeObserver{o}); e != nil {
		t.Fatal(e)
	}
	if m.updates != 0 || len(m.state.Events) != 0 || len(m.state.RuntimeObservations) != 1 || m.touches != 1 || m.fresh["op"].ObservedAt.Before(now) {
		t.Fatalf("unchanged poll wrote state: %+v", m)
	}
	for _, change := range []func(){func() { o.evidence.Details = "changed" }, func() { o.evidence.Healthy = false }, func() { m.state.Operations[0].GitOpsResult.CommitSHA = "other" }, func() { m.state.Operations[0].Artifact.Digest = "other" }} {
		before := len(m.state.RuntimeObservations)
		updates := m.updates
		change()
		if e := s.ObserveRuntimeOnce(ctx, []delivery.RuntimeObserver{o}); e != nil {
			t.Fatal(e)
		}
		if len(m.state.RuntimeObservations) != before+1 || m.updates != updates+1 {
			t.Fatal("change must record exactly once")
		}
	}
	if got := m.fresh["op"]; got.ObservationID != m.state.RuntimeObservations[len(m.state.RuntimeObservations)-1].ID || got.Healthy {
		t.Fatal("touch did not bind latest unhealthy observation")
	}
	if m.touches != 5 {
		t.Fatal(m.touches)
	}
}
func TestRuntimeRetentionBoundedScopedAndIdle(t *testing.T) {
	now := time.Now().UTC()
	m := &workerScopedCountingStore{workerCountingStore: &workerCountingStore{borrowedQueryStore: borrowedQueryStore{state: domain.EmptyState()}}}
	for i := 0; i < 5010; i++ {
		m.state.RuntimeObservations = append(m.state.RuntimeObservations, domain.RuntimeObservation{Meta: domain.Meta{ID: fmt.Sprintf("r%05d", i), ProductID: "p", CreatedAt: now.Add(-10 * 24 * time.Hour).Add(time.Duration(i) * time.Second)}, OperationID: "op", EnvironmentID: "env", ArtifactDigest: "digest"})
	}
	s := New(m)
	ctx := context.Background()
	if e := s.PruneRuntimeObservations(ctx, now, 168*time.Hour); e != nil {
		t.Fatal(e)
	}
	if len(m.state.RuntimeObservations) != 10 || m.updates != 1 || !reflect.DeepEqual(m.scopes[0], domain.StateMutationScope{MutableCollections: []string{"runtime_observations"}}) {
		t.Fatalf("cap or scope failed: %d %+v", len(m.state.RuntimeObservations), m.scopes)
	}
	if m.state.RuntimeObservations[0].ID != "r00000" || m.state.RuntimeObservations[9].ID != "r05009" {
		t.Fatal("lost historical root or latest")
	}
	if len(m.state.Events) != 1 || m.state.Events[0].Action != "prune_runtime_observations" {
		t.Fatal("missing summary")
	}
	if e := s.PruneRuntimeObservations(ctx, now, 168*time.Hour); e != nil {
		t.Fatal(e)
	}
	updates := m.updates
	if e := s.PruneRuntimeObservations(ctx, now, 168*time.Hour); e != nil {
		t.Fatal(e)
	}
	if m.updates != updates {
		t.Fatal("idle pass wrote")
	}
	if e := s.PruneRuntimeObservations(ctx, now, 0); e != nil || m.updates != updates {
		t.Fatal("disabled pass wrote")
	}
}

func TestRuntimeFreshnessQueryProjectionAndBaseline(t *testing.T) {
	s, m, _, c := releaseNotesFixture(t)
	at := time.Now().UTC()
	m.state.RuntimeObservations[0].CreatedAt = at.Add(-time.Hour)
	var request intelligenceRequest
	if e := decode(c.Data, &request); e != nil {
		t.Fatal(e)
	}
	if _, e := s.releaseNotesContext(&m.state, "p", request); e == nil {
		t.Fatal("stale baseline admitted without freshness")
	}
	s.runtimeFreshnessSnapshot = map[string]time.Time{m.state.RuntimeObservations[0].ID: at}
	if _, e := s.releaseNotesContext(&m.state, "p", request); e != nil {
		t.Fatalf("fresh unchanged baseline rejected: %v", e)
	}
	rows := observationViews(m.state.RuntimeObservations, s.runtimeFreshnessSnapshot)
	if !rows[0].ObservedAt.Equal(at) || !rows[0].CreatedAt.Equal(at.Add(-time.Hour)) {
		t.Fatal("projection overwrote provenance or lost freshness")
	}
}

func TestRuntimeRetentionReplansUnderLock(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-10 * 24 * time.Hour)
	m := &workerScopedCountingStore{workerCountingStore: &workerCountingStore{borrowedQueryStore: borrowedQueryStore{state: domain.EmptyState()}}}
	for i := 0; i < 3; i++ {
		m.state.RuntimeObservations = append(m.state.RuntimeObservations, domain.RuntimeObservation{Meta: domain.Meta{ID: fmt.Sprint(i), ProductID: "p", CreatedAt: old.Add(time.Duration(i) * time.Minute)}, OperationID: "op", ArtifactDigest: "digest"})
	}
	m.beforeClaim = func(st *domain.State) {
		st.ArtifactPins = append(st.ArtifactPins, domain.ArtifactPin{Meta: domain.Meta{ID: "pin", ProductID: "p"}, Digest: "digest"})
	}
	if e := New(m).PruneRuntimeObservations(context.Background(), now, 168*time.Hour); e != nil {
		t.Fatal(e)
	}
	if len(m.state.RuntimeObservations) != 3 || len(m.state.Events) != 0 {
		t.Fatal("writer did not replan against new pin")
	}
}

func TestRuntimeRetentionDoesNotExpandBorrowedBatch(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-10 * 24 * time.Hour)
	m := &workerScopedCountingStore{workerCountingStore: &workerCountingStore{borrowedQueryStore: borrowedQueryStore{state: domain.EmptyState()}}}
	for i := 0; i < 3; i++ {
		m.state.RuntimeObservations = append(m.state.RuntimeObservations, domain.RuntimeObservation{Meta: domain.Meta{ID: fmt.Sprint(i), ProductID: "p", CreatedAt: old.Add(time.Duration(i) * time.Minute)}, OperationID: "op", ArtifactDigest: "digest"})
	}
	m.beforeClaim = func(st *domain.State) {
		st.RuntimeObservations = append(st.RuntimeObservations, domain.RuntimeObservation{Meta: domain.Meta{ID: "new-candidate", ProductID: "p", CreatedAt: old.Add(90 * time.Second)}, OperationID: "op", ArtifactDigest: "digest"})
	}
	if err := New(m).PruneRuntimeObservations(context.Background(), now, 168*time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, r := range m.state.RuntimeObservations {
		if r.ID == "1" {
			t.Fatal("original candidate not removed")
		}
	}
	found := false
	for _, r := range m.state.RuntimeObservations {
		if r.ID == "new-candidate" {
			found = true
		}
	}
	if !found {
		t.Fatal("writer expanded borrowed candidate batch")
	}
}
