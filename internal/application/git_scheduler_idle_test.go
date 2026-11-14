package application

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
)

func gitSchedulerFixture(t *testing.T) domain.State {
	t.Helper()
	_, m, _ := externalFixture(t)
	in := integration(&m.state, "i")
	in.Owner = "agent/test"
	in.Status = "working"
	in.Branches = []domain.Branch{{RepositoryID: "source", Name: "feature/work"}}
	m.state.Events = nil
	return m.state
}

func TestGitSchedulersIdleScopeAndConcurrentEnqueue(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	for _, repository := range []bool{false, true} {
		for _, race := range []bool{false, true} {
			base := &workerCountingStore{borrowedQueryStore: borrowedQueryStore{state: gitSchedulerFixture(t)}}
			store := &workerScopedCountingStore{workerCountingStore: base}
			svc := NewWithProvider(store, &fakeDelivery{})
			schedule := svc.ScheduleGitRefresh
			planner := planGitRefresh
			if repository {
				schedule = svc.ScheduleRepositoryGitRefresh
				planner = planRepositoryGitRefresh
			}
			if race {
				base.beforeClaim = func(st *domain.State) {
					plan := planner(st, now, time.Minute)
					st.Operations = append(st.Operations, plan.operations...)
				}
			}
			if err := schedule(ctx, now, time.Minute); err != nil {
				t.Fatal(err)
			}
			if len(base.state.Operations) != 1 {
				t.Fatalf("duplicate/missing operations: %+v", base.state.Operations)
			}
			if !reflect.DeepEqual(store.scopes, []domain.StateMutationScope{{AppendCollections: []string{"operations"}}}) {
				t.Fatalf("scope: %+v", store.scopes)
			}
			if race && len(base.state.Events) != 0 {
				t.Fatal("race appended event")
			}
			if !race && (len(base.state.Events) != 1 || base.state.Events[0].EntityID != base.state.Operations[0].ID || base.state.Events[0].ID == "") {
				t.Fatal("missing provenance")
			}
			base.beforeClaim = nil
			if err := schedule(ctx, now, time.Minute); err != nil {
				t.Fatal(err)
			}
			if base.updates != 1 {
				t.Fatal("idle scheduler wrote")
			}
		}
	}
	empty := &workerCountingStore{}
	svc := NewWithProvider(empty, &fakeDelivery{})
	for _, schedule := range []func(context.Context, time.Time, time.Duration) error{svc.ScheduleGitRefresh, svc.ScheduleRepositoryGitRefresh, svc.ScheduleCompositionRefresh} {
		if err := schedule(ctx, now, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if empty.updates != 0 {
		t.Fatal("empty scheduler wrote")
	}
}

func TestGitRefreshPlannerWaitsAndOrdering(t *testing.T) {
	now := time.Now().UTC()
	for _, repository := range []bool{false, true} {
		for _, status := range []string{"PENDING", "RUNNING", "SUCCEEDED", "FAILED", "BLOCKED"} {
			for _, age := range []time.Duration{0, time.Minute, 5 * time.Minute} {
				st := gitSchedulerFixture(t)
				kind := "REFRESH_GIT"
				planner := planGitRefresh
				if repository {
					kind = "REFRESH_REPOSITORY_GIT"
					planner = planRepositoryGitRefresh
				}
				at := now.Add(-age)
				st.Operations = []domain.ExternalOperation{{Meta: domain.Meta{ProductID: "p", CreatedAt: at}, Kind: kind, IntegrationID: "i", RepositoryID: st.RepositoryBindings[0].RepositoryID, Status: status}}
				want := terminalOperation(status) && age >= time.Minute
				if status == "FAILED" || status == "BLOCKED" {
					want = age >= 5*time.Minute
				}
				plan := planner(&st, now, time.Minute)
				if (len(plan.operations) > 0) != want {
					t.Fatalf("repository=%v status=%s age=%v plan=%+v", repository, status, age, plan)
				}
			}
		}
		st := gitSchedulerFixture(t)
		for _, b := range st.RepositoryBindings {
			if repositoryGitInventoryRole(b.Role) {
				st.RepositoryBindings = []domain.RepositoryBinding{b}
				break
			}
		}
		planner := planGitRefresh
		cap := 5
		if repository {
			planner = planRepositoryGitRefresh
			cap = 3
		}
		for n := 1; n < 9; n++ {
			if repository {
				b := st.RepositoryBindings[0]
				b.ID = fmt.Sprint(n)
				b.RepositoryID = fmt.Sprint(n)
				st.RepositoryBindings = append(st.RepositoryBindings, b)
			} else {
				in := st.Integrations[0]
				in.ID = fmt.Sprint(n)
				st.Integrations = append(st.Integrations, in)
			}
		}
		plan := planner(&st, now, time.Minute)
		if len(plan.operations) != cap {
			t.Fatalf("cap: %d", len(plan.operations))
		}
		for n, op := range plan.operations {
			if repository {
				if op.RepositoryID != st.RepositoryBindings[n].RepositoryID {
					t.Fatal("repository order")
				}
			} else if op.IntegrationID != st.Integrations[n].ID {
				t.Fatal("integration order")
			}
		}
		// Existing pending work consumes global capacity even outside eligible scope.
		for n := 0; n < cap; n++ {
			st.Operations = append(st.Operations, domain.ExternalOperation{Kind: plan.operations[0].Kind, Status: "PENDING"})
		}
		if len(planner(&st, now, time.Minute).operations) != 0 {
			t.Fatal("active cap ignored")
		}
	}
}

func TestGitSchedulersLocalFallback(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Update(ctx, func(st *domain.State) error {
		events := st.Events
		*st = gitSchedulerFixture(t)
		st.Events = events
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewWithProvider(store, &fakeDelivery{})
	now := time.Now().UTC()
	for _, schedule := range []func(context.Context, time.Time, time.Duration) error{svc.ScheduleGitRefresh, svc.ScheduleRepositoryGitRefresh} {
		for n := 0; n < 2; n++ {
			if err := schedule(ctx, now, time.Minute); err != nil {
				t.Fatal(err)
			}
		}
	}
	st, err := store.Read(ctx)
	if err != nil || len(st.Operations) != 2 || len(st.Events) != 2 {
		t.Fatalf("operations=%d events=%d err=%v", len(st.Operations), len(st.Events), err)
	}
}

func TestGitRefreshPlannerLatestAndTimestamps(t *testing.T) {
	now := time.Now().UTC()
	for _, repository := range []bool{false, true} {
		st := gitSchedulerFixture(t)
		planner := planGitRefresh
		kind := "REFRESH_GIT"
		if repository {
			planner = planRepositoryGitRefresh
			kind = "REFRESH_REPOSITORY_GIT"
		}
		old := domain.ExternalOperation{Meta: domain.Meta{ProductID: "p", CreatedAt: now.Add(-time.Hour)}, Kind: kind, IntegrationID: "i", RepositoryID: "source", Status: "FAILED"}
		recent := old
		recent.CreatedAt = now.Add(-2 * time.Minute)
		recent.Status = "SUCCEEDED"
		for _, timestamp := range []string{"created", "updated", "finished"} {
			op := recent
			switch timestamp {
			case "updated":
				op.UpdatedAt = now
			case "finished":
				op.UpdatedAt = now.Add(-time.Hour)
				op.FinishedAt = &now
			}
			st.Operations = []domain.ExternalOperation{op, old}
			before := append([]domain.ExternalOperation(nil), st.Operations...)
			plan := planner(&st, now, time.Minute)
			if (len(plan.operations) > 0) != (timestamp == "created") {
				t.Fatalf("repository=%v timestamp=%s plan=%+v", repository, timestamp, plan)
			}
			if !reflect.DeepEqual(before, st.Operations) {
				t.Fatal("planner mutated state")
			}
		}
		// Equal creation timestamps retain the first operation, as before.
		old.CreatedAt = recent.CreatedAt
		old.UpdatedAt = now
		st.Operations = []domain.ExternalOperation{old, recent}
		if len(planner(&st, now, time.Minute).operations) != 0 {
			t.Fatal("equal timestamp ordering changed")
		}
	}
}
