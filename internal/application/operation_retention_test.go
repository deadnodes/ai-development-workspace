package application

import (
	"context"
	"fmt"
	"reflect"
	"releasecontrol/internal/domain"
	"testing"
	"time"
)

func TestOperationRetentionScopedBoundedIdle(t *testing.T) {
	now := time.Now().UTC()
	m := &workerScopedCountingStore{workerCountingStore: &workerCountingStore{borrowedQueryStore: borrowedQueryStore{state: domain.EmptyState()}}}
	for i := 0; i < 5010; i++ {
		op := fmt.Sprintf("op-%05d", i)
		m.state.Operations = append(m.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: op, CreatedAt: now.Add(-1000 * time.Hour).Add(time.Duration(i) * time.Second), UpdatedAt: now.Add(-1000 * time.Hour)}, Kind: "REFRESH_GIT", Status: "FAILED"})
		m.state.OperationSteps = append(m.state.OperationSteps, domain.OperationStep{Meta: domain.Meta{ID: "step-" + op}, OperationID: op})
	}
	journal := domain.Event{ID: "history", Action: "old", EntityID: "op-00000"}
	m.state.Events = []domain.Event{journal}
	s := New(m)
	ctx := context.Background()
	for pass, want := range []int{10, 1, 1} {
		if deleted, err := s.PruneOperations(ctx, now, 720*time.Hour, 168*time.Hour); err != nil {
			t.Fatal(err)
		} else if expected := []int{5000, 9, 0}[pass]; deleted != expected {
			t.Fatalf("deleted=%d want=%d", deleted, expected)
		}
		if len(m.state.Operations) != want || len(m.state.OperationSteps) != want {
			t.Fatalf("pass %d: ops=%d steps=%d", pass, len(m.state.Operations), len(m.state.OperationSteps))
		}
	}
	if m.updates != 2 || len(m.state.Events) != 3 || !reflect.DeepEqual(m.state.Events[0], journal) {
		t.Fatal("idle wrote or journal changed")
	}
	if !reflect.DeepEqual(m.scopes[0], domain.StateMutationScope{MutableCollections: []string{"operations", "operation_steps"}}) {
		t.Fatal(m.scopes)
	}
	if m.state.Events[1].Action != "prune_operations" || m.state.Events[1].Data.Data["deleted_steps"] != 5000 {
		t.Fatal("summary")
	}
	if _, err := s.PruneOperations(ctx, now, 0, 168*time.Hour); err != nil || m.updates != 2 {
		t.Fatal("disabled wrote")
	}
}
func TestOperationRetentionRevalidates(t *testing.T) {
	now := time.Now().UTC()
	m := &workerScopedCountingStore{workerCountingStore: &workerCountingStore{borrowedQueryStore: borrowedQueryStore{state: domain.EmptyState()}}}
	for _, name := range []string{"a", "b"} {
		m.state.Operations = append(m.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: name, CreatedAt: now.Add(-1000 * time.Hour), UpdatedAt: now.Add(-1000 * time.Hour)}, Status: "FAILED"})
	}
	m.beforeClaim = func(st *domain.State) { st.RuntimeObservations = []domain.RuntimeObservation{{OperationID: "a"}} }
	if _, err := New(m).PruneOperations(context.Background(), now, 720*time.Hour, 168*time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Operations) != 2 || len(m.state.Events) != 0 {
		t.Fatal("racing reference lost")
	}
}

func TestOperationRetentionRefreshWindow(t *testing.T) {
	for _, refresh := range []time.Duration{0, 168 * time.Hour, 900 * time.Hour} {
		t.Run(refresh.String(), func(t *testing.T) {
			now := time.Now().UTC()
			m := &workerScopedCountingStore{workerCountingStore: &workerCountingStore{borrowedQueryStore: borrowedQueryStore{state: domain.EmptyState()}}}
			for _, kind := range []string{"REFRESH_GIT", "REFRESH_REPOSITORY_GIT", "OTHER"} {
				for _, row := range []struct {
					id          string
					age         time.Duration
					status, key string
				}{
					{"old", 800 * time.Hour, "FAILED", ""},
					{"week", 200 * time.Hour, "FAILED", ""},
					{"boundary", 168 * time.Hour, "FAILED", ""},
					{"running", 800 * time.Hour, "RUNNING", ""},
					{"keyed", 800 * time.Hour, "FAILED", "receipt"},
					{"referenced", 800 * time.Hour, "FAILED", ""},
					{"latest", 1000 * time.Hour, "FAILED", ""},
				} {
					created := now.Add(-row.age)
					if row.id == "latest" {
						created = now
					}
					m.state.Operations = append(m.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: kind + row.id, CreatedAt: created, UpdatedAt: now.Add(-row.age)}, Kind: kind, Status: row.status, RequestKey: row.key})
				}
				m.state.RuntimeObservations = append(m.state.RuntimeObservations, domain.RuntimeObservation{OperationID: kind + "referenced"})
			}
			deleted, err := New(m).PruneOperations(context.Background(), now, 720*time.Hour, refresh)
			if err != nil {
				t.Fatal(err)
			}
			want := 3
			if refresh == 168*time.Hour {
				want = 5
			}
			if refresh == 900*time.Hour {
				want = 1
			}
			if deleted != want {
				t.Fatalf("deleted=%d want=%d", deleted, want)
			}
			for _, op := range m.state.Operations {
				if op.ID == "OTHERweek" {
					continue
				}
				if refresh == 168*time.Hour && (op.ID == "REFRESH_GITweek" || op.ID == "REFRESH_REPOSITORY_GITweek") {
					t.Fatal("refresh survived shorter window")
				}
			}
		})
	}
}
