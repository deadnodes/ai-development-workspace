package application

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
)

type workerCountingStore struct {
	borrowedQueryStore
	updates     int
	beforeClaim func(*domain.State)
	afterClaim  func(*domain.State) error
}

func (s *workerCountingStore) Update(_ context.Context, fn func(*domain.State) error) error {
	s.updates++
	if s.beforeClaim != nil {
		s.beforeClaim(&s.state)
	}
	if err := fn(&s.state); err != nil {
		return err
	}
	if s.afterClaim != nil {
		return s.afterClaim(&s.state)
	}
	return nil
}

type workerScopedCountingStore struct {
	*workerCountingStore
	scopes []domain.StateMutationScope
}

func (s *workerScopedCountingStore) UpdateScoped(ctx context.Context, scope domain.StateMutationScope, fn func(*domain.State) error) error {
	s.scopes = append(s.scopes, scope)
	return s.workerCountingStore.Update(ctx, fn)
}
func workerOp(id, kind string, at time.Time) domain.ExternalOperation {
	return domain.ExternalOperation{Meta: domain.Meta{ID: id}, Kind: kind, Status: "PENDING", NextAttemptAt: at}
}
func TestWorkerIdleDoesNotUpdate(t *testing.T) {
	now := time.Now()
	for _, ops := range [][]domain.ExternalOperation{
		nil,
		{workerOp("future", "DEPLOY", now.Add(time.Hour))},
		{{Kind: "DEPLOY", Status: "SUCCEEDED"}},
		{{Kind: "DEPLOY", Status: "RUNNING", LeaseUntil: now.Add(time.Hour)}},
		{workerOp("external", domain.WorkerOperationKind, now), workerOp("registry", domain.RegistryObserveOperationKind, now), workerOp("intelligence", domain.IntelligenceOperationKind, now)},
	} {
		store := &workerScopedCountingStore{workerCountingStore: &workerCountingStore{borrowedQueryStore: borrowedQueryStore{state: domain.State{Operations: ops}}}}
		worked, err := NewWithProvider(store, &fakeDelivery{}).Tick(context.Background())
		if err != nil || worked || store.updates != 0 || len(store.scopes) != 0 {
			t.Fatalf("idle tick: worked=%v err=%v updates=%d scopes=%v", worked, err, store.updates, store.scopes)
		}
	}
}
func TestWorkerClaimPreservesSemanticsAndScope(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, started := range []*time.Time{nil, copyTime(&time.Time{})} {
			base := &workerCountingStore{borrowedQueryStore: borrowedQueryStore{state: domain.State{Operations: []domain.ExternalOperation{workerOp("op", "DEPLOY", time.Time{})}}}}
			base.state.Operations[0].Attempts = 2
			base.state.Operations[0].StartedAt = started
			stop := errors.New("stop after claim")
			before := time.Now().UTC()
			base.afterClaim = func(st *domain.State) error {
				op := st.Operations[0]
				if op.Status != "RUNNING" || op.Attempts != 3 || op.LeaseOwner == "" || op.StartedAt == nil {
					t.Fatalf("claim: %+v", op)
				}
				if started != nil && !op.StartedAt.Equal(*started) {
					t.Fatal("replaced StartedAt")
				}
				if started == nil && (op.StartedAt.Before(before) || op.StartedAt.After(time.Now())) {
					t.Fatal("invalid StartedAt")
				}
				if op.LeaseUntil.Before(before.Add(45*time.Second)) || op.LeaseUntil.After(time.Now().Add(45*time.Second)) {
					t.Fatal("invalid lease")
				}
				return stop
			}
			var store Store = base
			wrapper := &workerScopedCountingStore{workerCountingStore: base}
			if scoped {
				store = wrapper
			}
			worked, err := NewWithProvider(store, &fakeDelivery{}).Tick(context.Background())
			if worked || !errors.Is(err, stop) || base.updates != 1 {
				t.Fatalf("worked=%v err=%v updates=%d", worked, err, base.updates)
			}
			if scoped && !reflect.DeepEqual(wrapper.scopes, []domain.StateMutationScope{{MutableCollections: []string{"operations"}}}) {
				t.Fatalf("scopes: %+v", wrapper.scopes)
			}
		}
	}
}
func TestWorkerReselectsAfterConcurrentLease(t *testing.T) {
	store := &workerScopedCountingStore{workerCountingStore: &workerCountingStore{borrowedQueryStore: borrowedQueryStore{state: domain.State{Operations: []domain.ExternalOperation{workerOp("op", "DEPLOY", time.Time{})}}}}}
	store.beforeClaim = func(st *domain.State) {
		st.Operations[0].LeaseOwner = "other"
		st.Operations[0].LeaseUntil = time.Now().Add(time.Minute)
	}
	worked, err := NewWithProvider(store, &fakeDelivery{}).Tick(context.Background())
	op := store.state.Operations[0]
	if err != nil || worked || store.updates != 1 || op.LeaseOwner != "other" || op.Attempts != 0 || op.StartedAt != nil || op.Status != "PENDING" {
		t.Fatalf("worked=%v err=%v op=%+v", worked, err, op)
	}
}
func TestSelectWorkerCandidatePriorityAndAvailability(t *testing.T) {
	now := time.Now().UTC()
	st := domain.State{Operations: []domain.ExternalOperation{
		workerOp("refresh", "REFRESH_GIT", now.Add(-time.Hour)),
		workerOp("later", "DEPLOY", now.Add(-time.Minute)),
		workerOp("b", "DEPLOY", now.Add(-time.Hour)),
		workerOp("a", "DEPLOY", now.Add(-time.Hour)),
	}}
	for _, want := range []string{"a", "b", "later", "refresh"} {
		index, ok := selectWorkerCandidate(&st, now, true, false, false)
		if !ok || st.Operations[index].ID != want {
			t.Fatalf("want %s, index=%d ok=%v", want, index, ok)
		}
		st.Operations[index].Status = "SUCCEEDED"
	}
	st.Operations = []domain.ExternalOperation{workerOp("runtime", "REFRESH_RUNTIME", now), workerOp("ai", domain.IntelligenceOperationKind, now), workerOp("deploy", "DEPLOY", now)}
	for _, tc := range []struct {
		provider, runtime, intelligence bool
		want                            string
	}{{false, false, false, ""}, {false, true, false, "runtime"}, {false, false, true, "ai"}, {true, false, false, "deploy"}} {
		index, ok := selectWorkerCandidate(&st, now, tc.provider, tc.runtime, tc.intelligence)
		if ok != (tc.want != "") || ok && st.Operations[index].ID != tc.want {
			t.Fatalf("availability %+v: index=%d ok=%v", tc, index, ok)
		}
	}
}
func TestWorkerLocalStoreViewAndFallback(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewWithProvider(store, &fakeDelivery{})
	if worked, err := s.Tick(ctx); err != nil || worked {
		t.Fatalf("idle: %v %v", worked, err)
	}
	if err := store.Update(ctx, func(st *domain.State) error {
		st.Operations = append(st.Operations, workerOp("op", "INVALID", time.Time{}))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.Tick(ctx); err != nil || !worked {
		t.Fatalf("runnable: %v %v", worked, err)
	}
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	op := st.Operations[0]
	if op.Attempts != 1 || op.StartedAt == nil || op.Status != "FAILED" {
		t.Fatalf("local claim/finish: %+v", op)
	}
}
