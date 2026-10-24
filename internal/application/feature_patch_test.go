package application

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
)

func featurePatchFixture(t *testing.T) (*Service, *memoryStore, *fakeFlow) {
	t.Helper()
	s, m, f := flowFixture(t)
	environmentBinding(&m.state, "dev", "component").GitOpsMode = "PR"
	exec(t, s, domain.Command{Action: "create_feature", ID: "fb", ProductID: "p", Data: map[string]any{"title": "B", "goal": "B"}})
	exec(t, s, domain.Command{Action: "create_integration", ID: "ib", FeatureID: "fb", Data: map[string]any{"title": "B", "objective": "B", "repositories": []string{"source"}}})
	for _, rev := range []struct{ id, in, head string }{{"b1", "ib", "e"}, {"a2", "i", "c"}, {"b2", "ib", "d"}} {
		exec(t, s, domain.Command{Action: "record_integration_revision", ID: rev.id, IntegrationID: rev.in, Data: map[string]any{"repository_id": "source", "branch": "feature/" + rev.in, "base_commit": shaBase, "head_commit": strings.Repeat(rev.head, 40), "commits": []string{strings.Repeat(rev.head, 40)}}})
	}
	flowPlan(t, s, "baseline", "dev", "rev", "b1")
	exec(t, s, domain.Command{Action: "select_composition", ID: "baseline"})
	return s, m, f
}
func featurePatchCommand(st domain.State, feature, revision, key string) domain.Command {
	env := environmentByID(&st, "dev")
	return domain.Command{Action: "update_environment_feature", Actor: "agent", FeatureID: feature, Data: map[string]any{"environment_id": "dev", "revision_ids": []string{revision}, "idempotency_key": key, "expected_selection": selectionToken(*env)}}
}
func featurePatchState(t *testing.T, st domain.State, a, b string) {
	t.Helper()
	env := environmentByID(&st, "dev")
	comp := compositionByID(&st, env.DesiredCompositionID)
	if comp.AutoRefresh || !reflect.DeepEqual(comp.Components[0].RevisionIDs, []string{a, b}) || comp.Components[0].BaseCommit != shaBase {
		t.Fatalf("unexpected pinned composition %+v", comp)
	}
}
func TestFeaturePatchPreservesPinnedInputsAndHistoricalReceipt(t *testing.T) {
	s, m, _ := featurePatchFixture(t)
	baseline, _ := json.Marshal(m.state.Compositions[0])
	m.state.GitObservations = append(m.state.GitObservations, domain.GitObservation{Meta: domain.Meta{ProductID: "p"}, RepositoryID: "source", MainCommit: strings.Repeat("f", 40), ObservedAt: time.Now()})
	cmd := featurePatchCommand(m.state, "f", "a2", "patch-a")
	op := exec(t, s, cmd).(domain.ExternalOperation)
	featurePatchState(t, m.state, "a2", "b1")
	if op.Kind != "COMPOSE" || op.FeatureUpdate == nil || op.FeatureID != "f" || op.RequestHash == "" {
		t.Fatalf("missing receipt %+v", op)
	}
	frozen, _ := json.Marshal(m.state.Compositions[0])
	if string(frozen) != string(baseline) {
		t.Fatal("old composition mutated")
	}
	exec(t, s, featurePatchCommand(m.state, "fb", "b2", "patch-b"))
	before := featurePatchFingerprint(m.state)
	replay := exec(t, s, cmd).(domain.ExternalOperation)
	after := featurePatchFingerprint(m.state)
	if replay.ID != op.ID || string(before) != string(after) {
		t.Fatal("historical replay mutated state")
	}
	cmd.Actor = "other"
	if _, err := s.Execute(context.Background(), cmd); err == nil {
		t.Fatal("actor conflict accepted")
	}
	cmd.Actor = "agent"
	cmd.Data["revision_ids"] = []string{"rev"}
	reject(t, s, cmd)
	// No unchanged patch queues another operation/receipt.
	reject(t, s, featurePatchCommand(m.state, "f", "a2", "unchanged"))
	for i := range m.state.Operations {
		m.state.Operations[i].Status = "SUCCEEDED"
	}
	if err := s.ScheduleCompositionRefresh(context.Background(), time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	featurePatchState(t, m.state, "a2", "b2")
}
func TestFeaturePatchRejectsAtomically(t *testing.T) {
	cases := map[string]func(*memoryStore, *domain.Command){
		"irrelevant integration": func(m *memoryStore, c *domain.Command) { c.IntegrationID = "i" },
		"irrelevant gate":        func(m *memoryStore, c *domain.Command) { c.GateID = "gate" },
		"irrelevant check":       func(m *memoryStore, c *domain.Command) { c.CheckID = "check" },
		"irrelevant result":      func(m *memoryStore, c *domain.Command) { c.ResultID = "result" },
		"missing CAS":            func(m *memoryStore, c *domain.Command) { delete(c.Data, "expected_selection") },
		"missing generation": func(m *memoryStore, c *domain.Command) {
			c.Data["expected_selection"] = map[string]any{"composition_id": "baseline", "operation_id": ""}
		},
		"stale":           func(m *memoryStore, c *domain.Command) { environmentByID(&m.state, "dev").SelectionGeneration++ },
		"foreign feature": func(m *memoryStore, c *domain.Command) { c.FeatureID = "fb" },
		"duplicate":       func(m *memoryStore, c *domain.Command) { c.Data["revision_ids"] = []string{"a2", "a2"} },
		"production": func(m *memoryStore, c *domain.Command) {
			environmentBinding(&m.state, "dev", "component").Purpose = "PROD"
		},
		"direct": func(m *memoryStore, c *domain.Command) {
			environmentBinding(&m.state, "dev", "component").GitOpsMode = "DIRECT"
		},
		"disabled": func(m *memoryStore, c *domain.Command) {
			environmentBinding(&m.state, "dev", "component").AllowDeploy = false
		},
		"overflow": func(m *memoryStore, c *domain.Command) {
			environmentByID(&m.state, "dev").SelectionGeneration = math.MaxUint64
			c.Data["expected_selection"] = selectionToken(*environmentByID(&m.state, "dev"))
		},
		"uncertain": func(m *memoryStore, c *domain.Command) {
			m.state.Operations = append(m.state.Operations, domain.ExternalOperation{EnvironmentID: "dev", Phase: "APPLY_UNCERTAIN", Status: "RUNNING", LeaseUntil: time.Now().Add(time.Minute)})
		},
		"released": func(m *memoryStore, c *domain.Command) { integration(&m.state, "i").Status = "released" },
		"missing baseline": func(m *memoryStore, c *domain.Command) {
			environmentByID(&m.state, "dev").DesiredCompositionID = ""
			c.Data["expected_selection"] = selectionToken(*environmentByID(&m.state, "dev"))
		},
		"dependency": func(m *memoryStore, c *domain.Command) {
			integration(&m.state, "i").Dependencies = []string{"not-selected"}
		},
		"new repo": func(m *memoryStore, c *domain.Command) { revisionByID(&m.state, "a2").RepositoryID = "another" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s, m, _ := featurePatchFixture(t)
			c := featurePatchCommand(m.state, "f", "a2", "reject")
			change(m, &c)
			before := featurePatchFingerprint(m.state)
			reject(t, s, c)
			after := featurePatchFingerprint(m.state)
			if string(before) != string(after) {
				t.Fatal("rejection mutated state")
			}
		})
	}
}
func TestFeaturePatchSelectionGenerationABAResetAndObservedState(t *testing.T) {
	s, m, _ := featurePatchFixture(t)
	cmd := featurePatchCommand(m.state, "f", "a2", "aba")
	env := environmentByID(&m.state, "dev")
	env.Runtime = map[string]any{"healthy": true}
	env.Reconciled = map[string]any{"commit": "old"}
	exec(t, s, domain.Command{Action: "select_composition", ID: "baseline"})
	if _, err := s.Execute(context.Background(), cmd); !errors.Is(err, ErrSelectionChanged) {
		t.Fatalf("ABA accepted: %v", err)
	}
	env = environmentByID(&m.state, "dev")
	generation := env.SelectionGeneration
	exec(t, s, domain.Command{Action: "update_environment", ID: "dev", Data: map[string]any{"namespace": "new"}})
	env = environmentByID(&m.state, "dev")
	if env.SelectionGeneration != generation+1 || env.DesiredCompositionID != "" || env.Runtime["healthy"] != true || env.Reconciled["commit"] != "old" {
		t.Fatalf("bad reset %+v", env)
	}
	reject(t, s, domain.Command{Action: "update_environment", ID: "dev", Data: map[string]any{"selection_generation": 0}})
}

type staleFeaturePatchStore struct {
	Store
	once   sync.Once
	mutate func()
}

func (s *staleFeaturePatchStore) Read(ctx context.Context) (domain.State, error) {
	st, err := s.Store.Read(ctx)
	s.once.Do(s.mutate)
	return st, err
}
func TestFeaturePatchSchedulerStaleAndFreshTicks(t *testing.T) {
	s, m, f := featurePatchFixture(t)
	baseline := compositionByID(&m.state, "baseline")
	baseline.AutoRefresh = true
	c := featurePatchCommand(m.state, "f", "a2", "pin")
	// Scheduler reads the old refreshable baseline, then a user pins A2+B1.
	wrapper := &staleFeaturePatchStore{Store: m, mutate: func() { exec(t, s, c) }}
	scheduler := NewWithProvider(wrapper, f)
	if err := scheduler.ScheduleCompositionRefresh(context.Background(), time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Operations) != 1 {
		t.Fatal("stale scheduler queued work")
	}
	m.state.Operations[0].Status = "SUCCEEDED"
	before := featurePatchFingerprint(m.state)
	if err := s.ScheduleCompositionRefresh(context.Background(), time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	after := featurePatchFingerprint(m.state)
	if string(before) != string(after) {
		t.Fatal("fresh scheduler changed pinned terminal composition")
	}
	featurePatchState(t, m.state, "a2", "b1")
}
func TestFeaturePatchLocalConcurrentCASReplayReopenRestore(t *testing.T) {
	ctx := context.Background()
	_, m, _ := featurePatchFixture(t)
	path := filepath.Join(t.TempDir(), "rcp.db")
	store, err := persistence.OpenLocal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	if err := store.Update(ctx, func(st *domain.State) error { *st = m.state; return nil }); err != nil {
		t.Fatal(err)
	}
	s := New(store)
	a := featurePatchCommand(m.state, "f", "a2", "concurrent-a")
	b := featurePatchCommand(m.state, "fb", "b2", "concurrent-b")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, c := range []domain.Command{a, b} {
		wg.Add(1)
		go func(c domain.Command) { defer wg.Done(); _, e := s.Execute(ctx, c); results <- e }(c)
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else if errors.Is(e, ErrSelectionChanged) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
	st, _ := store.Read(ctx)
	if len(st.Operations) != 1 || len(st.Compositions) != 2 {
		t.Fatal("CAS loser wrote records")
	}
	winning := a
	losing := b
	if st.Operations[0].FeatureID == "fb" {
		winning = b
		losing = a
	}
	losing.Data["expected_selection"] = selectionToken(*environmentByID(&st, "dev"))
	if _, err = s.Execute(ctx, losing); err != nil {
		t.Fatal(err)
	}
	st, _ = store.Read(ctx)
	featurePatchState(t, st, "a2", "b2")
	before := featurePatchFingerprint(st)
	store.Close()
	store, err = persistence.OpenLocal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s = New(store)
	if _, err = s.Execute(ctx, winning); err != nil {
		t.Fatal(err)
	}
	st, _ = store.Read(ctx)
	after := featurePatchFingerprint(st)
	if string(before) != string(after) {
		t.Fatal("reopen replay mutated state")
	}
	backup, err := s.CreateBackup(ctx, "tester")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	rs := New(restored)
	if _, err = rs.RestoreBackup(ctx, "tester", backup.ArchiveBase64, backup.SHA256); err != nil {
		t.Fatal(err)
	}
	restoredState, _ := restored.Read(ctx)
	before = featurePatchFingerprint(restoredState)
	if _, err = rs.Execute(ctx, winning); err != nil {
		t.Fatal(err)
	}
	restoredState, _ = restored.Read(ctx)
	after = featurePatchFingerprint(restoredState)
	if string(before) != string(after) {
		t.Fatal("restored replay mutated state")
	}
	// Concurrent identical accepted requests remain historical reads.
	wg = sync.WaitGroup{}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := rs.Execute(ctx, winning); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	restoredState, _ = restored.Read(ctx)
	after = featurePatchFingerprint(restoredState)
	if string(before) != string(after) {
		t.Fatal("duplicate replay mutated state")
	}
}

func featurePatchFingerprint(value any) []byte {
	b, _ := json.Marshal(value)
	var canonical any
	_ = json.Unmarshal(b, &canonical)
	b, _ = json.Marshal(canonical)
	return b
}

func TestFeaturePatchMonorepoAppCohortAndAppendOrder(t *testing.T) {
	s, m, _ := featurePatchFixture(t)
	exec(t, s, domain.Command{Action: "create_application", ID: "web", ProductID: "p", Data: map[string]any{"name": "Web", "repository_id": "source", "path": "web"}})
	// Both applications are explicitly selected before patching.
	baseline := compositionByID(&m.state, "baseline")
	copyComponent := baseline.Components[0]
	copyComponent.ApplicationID = "web"
	baseline.Components = append(baseline.Components, copyComponent)
	baseline.ApplicationSnapshots = append(baseline.ApplicationSnapshots, *applicationByID(&m.state, "web"))
	build := *componentBuild(&m.state, "component")
	build.ID = "web-build"
	build.ApplicationID = "web"
	m.state.ComponentBuilds = append(m.state.ComponentBuilds, build)
	target := *environmentBinding(&m.state, "dev", "component")
	target.ID = "web-target"
	target.ApplicationID = "web"
	target.Path = "web.yaml"
	m.state.EnvironmentBindings = append(m.state.EnvironmentBindings, target)
	exec(t, s, domain.Command{Action: "create_integration", ID: "i-extra", FeatureID: "f", Data: map[string]any{"title": "Extra", "objective": "Extra", "repositories": []string{"source"}}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "extra", IntegrationID: "i-extra", Data: map[string]any{"repository_id": "source", "branch": "feature/extra", "base_commit": shaBase, "head_commit": strings.Repeat("9", 40), "commits": []string{strings.Repeat("9", 40)}}})
	c := featurePatchCommand(m.state, "f", "a2", "monorepo")
	c.Data["revision_ids"] = []string{"extra", "a2"}
	op := exec(t, s, c).(domain.ExternalOperation)
	components := op.CompositionSnapshot.Components
	want := []string{"a2", "b1", "extra"}
	if len(components) != 2 || !reflect.DeepEqual(components[0].RevisionIDs, want) || !reflect.DeepEqual(components[1].RevisionIDs, want) || components[0].TargetBranch != components[1].TargetBranch {
		t.Fatalf("monorepo order/cohort differs %+v", components)
	}
}
func TestFeaturePatchDelayedOldBuildCannotPublish(t *testing.T) {
	s, m, f := featurePatchFixture(t)
	old := exec(t, s, domain.Command{Action: "reconcile_composition", ID: "baseline"}).(domain.ExternalOperation)
	flowTickUntil(t, s, m, old.ID, func(op domain.ExternalOperation) bool { return len(op.ChildIDs) > 0 })
	// Let the obsolete child reach build dispatch, then replace its parent intent.
	flowTickUntil(t, s, m, old.ID, func(op domain.ExternalOperation) bool { return len(f.builds) > 0 })
	latest := exec(t, s, featurePatchCommand(m.state, "f", "a2", "supersede")).(domain.ExternalOperation)
	// Restrict scheduler to old work by making the new parent temporarily dormant.
	operationByID(&m.state, latest.ID).NextAttemptAt = time.Now().Add(time.Hour)
	flowTickUntil(t, s, m, old.ID, func(op domain.ExternalOperation) bool { return terminalOperation(op.Status) })
	for _, op := range m.state.Operations {
		if op.ParentID == old.ID && op.GitOpsPR != nil {
			t.Fatal("old build proposed a GitOps write")
		}
	}
	if f.applyCounts["app.yaml"] != 0 || environmentByID(&m.state, "dev").DesiredOperationID != latest.ID {
		t.Fatal("old build changed latest intent")
	}
	featurePatchState(t, m.state, "a2", "b1")
}
