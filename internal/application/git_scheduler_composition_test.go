package application

import (
	"context"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

type compositionRefreshRaceStore struct {
	Store
	mutate func()
}

func (s *compositionRefreshRaceStore) View(ctx context.Context, fn func(*domain.State) error) error {
	st, err := s.Store.Read(ctx)
	if err != nil {
		return err
	}
	err = fn(&st)
	if s.mutate != nil {
		mutate := s.mutate
		s.mutate = nil
		mutate()
	}
	return err
}

func TestCompositionRefreshAllEligibleEnvironments(t *testing.T) {
	for _, race := range []bool{false, true} {
		name := "both stale"
		if race {
			name = "first selection races"
		}
		t.Run(name, func(t *testing.T) {
			s, m, provider := featurePatchFixture(t)
			flowEnvironment(t, s, "test", "TEST")
			flowPlan(t, s, "test-baseline", "test", "rev", "b1")
			parents := map[string]string{}
			for _, target := range []struct{ env, composition string }{{"dev", "baseline"}, {"test", "test-baseline"}} {
				compositionByID(&m.state, target.composition).AutoRefresh = true
				parent := exec(t, s, domain.Command{Action: "reconcile_composition", ID: target.composition}).(domain.ExternalOperation)
				operationByID(&m.state, parent.ID).Status = "SUCCEEDED"
				parents[target.env] = parent.ID
			}
			scheduler := s
			if race {
				scheduler = NewWithProvider(&compositionRefreshRaceStore{Store: m, mutate: func() {
					environmentByID(&m.state, "dev").SelectionGeneration++
				}}, provider)
			}
			before := len(m.state.Operations)
			if err := scheduler.ScheduleCompositionRefresh(context.Background(), time.Now(), time.Minute); err != nil {
				t.Fatal(err)
			}
			want := 2
			if race {
				want = 1
			}
			if got := len(m.state.Operations) - before; got != want {
				t.Fatalf("queued %d refreshes, want %d", got, want)
			}
			seen := map[string]bool{}
			for _, op := range m.state.Operations[before:] {
				if seen[op.EnvironmentID] || op.Kind != "COMPOSE" || op.SourceRetentionParentID != parents[op.EnvironmentID] {
					t.Fatalf("unexpected refresh: %+v", op)
				}
				seen[op.EnvironmentID] = true
				comp := compositionByID(&m.state, environmentByID(&m.state, op.EnvironmentID).DesiredCompositionID)
				if comp == nil || !comp.AutoRefresh || len(comp.RevisionSnapshots) != 2 || comp.RevisionSnapshots[0].ID != "a2" || comp.RevisionSnapshots[1].ID != "b2" {
					t.Fatalf("refresh did not capture latest revisions: %+v", comp)
				}
			}
			if !seen["test"] || (!race && !seen["dev"]) {
				t.Fatalf("missing refreshed environment: %v", seen)
			}
		})
	}
}
