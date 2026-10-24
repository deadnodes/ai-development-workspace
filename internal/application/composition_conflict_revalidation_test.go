package application

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

func conflictEvidenceCommand(action, conflictID string, data map[string]any) domain.Command {
	if data == nil {
		data = map[string]any{}
	}
	data["conflict_id"] = conflictID
	return domain.Command{Action: action, Actor: "agent/resolver", ProductID: "p", Data: data}
}

func resolvedConflictEvidenceFixture(t *testing.T, sameFeature bool) (domain.State, string) {
	t.Helper()
	var st domain.State
	var original string
	if !sameFeature {
		st, original = callerConflictFixture(t)
	} else {
		s, m, provider := flowFixture(t)
		exec(t, s, domain.Command{Action: "create_integration", ID: "caller-i2", FeatureID: "f", Data: map[string]any{"title": "Second slice", "objective": "Preserve B"}})
		exec(t, s, domain.Command{Action: "record_integration_revision", ID: "caller-rev2", IntegrationID: "caller-i2", Data: map[string]any{"repository_id": "source", "branch": "feature/second", "base_commit": shaBase, "head_commit": strings.Repeat("e", 40), "commits": []string{strings.Repeat("e", 40)}}})
		for n, iid := range []string{"i", "caller-i2"} {
			exec(t, s, domain.Command{Action: "create_gate", ID: fmt.Sprintf("caller-g%d", n), FeatureID: "f", Data: map[string]any{"title": "Acceptance", "reason": "Both slices", "integration_ids": []string{iid}}})
			exec(t, s, domain.Command{Action: "add_check", ID: fmt.Sprintf("caller-check%d", n), GateID: fmt.Sprintf("caller-g%d", n), Data: map[string]any{"title": "Check", "mechanism": "automated"}})
		}
		provider.conflict = true
		flowPlan(t, s, "caller-composition", "dev", "rev", "caller-rev2")
		original = flowStart(t, s, m, "caller-composition")
		flowTickUntil(t, s, m, original, func(o domain.ExternalOperation) bool { return terminalOperation(o.Status) })
		st = m.state
	}
	m := &memoryStore{state: st}
	s := New(m)
	cid := st.CompositionConflicts[0].ID
	exec(t, s, conflictEvidenceCommand("claim_composition_conflict", cid, nil))
	exec(t, s, conflictEvidenceCommand("record_conflict_resolution", cid, map[string]any{"commit": strings.Repeat("c", 40), "rationale": "Preserve both exact inputs"}))
	ids := []string{}
	for _, check := range []string{"caller-check0", "caller-check1"} {
		r := exec(t, s, domain.Command{Action: "record_check_result", CheckID: check, Data: map[string]any{"result": "passed", "commit": strings.Repeat("c", 40)}}).(domain.CheckResult)
		ids = append(ids, r.ID)
	}
	exec(t, s, conflictEvidenceCommand("verify_conflict_resolution", cid, map[string]any{"result_ids": ids}))
	return m.state, original
}

func conflictEvidenceReconcile(st domain.State) domain.Command {
	c := st.CompositionConflicts[0]
	return domain.Command{Action: "reconcile_composition", Actor: "agent/requester", ProductID: "p", ID: c.CompositionID, Data: map[string]any{"resolution_conflict_ids": []string{c.ID}}}
}

func TestConflictEvidenceRecheckedAtAdmissionAndQueuedGuards(t *testing.T) {
	seed, _ := resolvedConflictEvidenceFixture(t, false)
	m := &memoryStore{state: seed}
	queued := exec(t, New(m), conflictEvidenceReconcile(seed)).(domain.ExternalOperation)
	seed = m.state
	if err := flowGuard(&seed, queued); err != nil {
		t.Fatal("fresh queued control", err)
	}
	if err := flowChildAllowed(&seed, domain.ExternalOperation{ParentID: queued.ID, EnvironmentID: queued.EnvironmentID}); err != nil {
		t.Fatal("fresh queued child control", err)
	}
	cases := map[string]func(*domain.State){
		"missing result": func(st *domain.State) { st.Results = st.Results[1:] },
		"duplicate reference": func(st *domain.State) {
			st.CompositionConflicts[0].ResultIDs = append(st.CompositionConflicts[0].ResultIDs, st.CompositionConflicts[0].ResultIDs[0])
		},
		"duplicate result identity": func(st *domain.State) { st.Results = append(st.Results, st.Results[0]) },
		"foreign result":            func(st *domain.State) { st.Results[0].ProductID = "other" },
		"wrong commit":              func(st *domain.State) { st.Results[0].Commit = strings.Repeat("d", 40) },
		"wrong feature":             func(st *domain.State) { st.Results[0].FeatureID = "caller-f2" },
		"missing check":             func(st *domain.State) { st.Checks = st.Checks[1:] },
		"foreign check":             func(st *domain.State) { st.Checks[0].ProductID = "other" },
		"check feature changed":     func(st *domain.State) { st.Checks[0].FeatureID = "caller-f2" },
		"dangling check gate":       func(st *domain.State) { st.Checks[0].GateID = "missing" },
		"foreign gate":              func(st *domain.State) { st.Gates[0].ProductID = "other" },
		"gate feature changed":      func(st *domain.State) { st.Gates[0].FeatureID = "caller-f2" },
		"lost integration coverage": func(st *domain.State) { st.Gates[0].IntegrationIDs = []string{"caller-i2"} },
		"integration removed":       func(st *domain.State) { st.Integrations = st.Integrations[1:] },
		"new blocking check": func(st *domain.State) {
			check := st.Checks[0]
			check.ID = "additional"
			st.Checks = append(st.Checks, check)
		},
		"empty blocking gate": func(st *domain.State) { gate := st.Gates[0]; gate.ID = "additional"; st.Gates = append(st.Gates, gate) },
	}
	for _, result := range []string{"failed", "blocked", "skipped", "passed"} {
		cases["superseded by "+result] = func(st *domain.State) {
			r := st.Results[0]
			r.ID, r.Result, r.CreatedAt = "newest", result, time.Time{}
			st.Results = append(st.Results, r)
		}
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			st := cloneCallerState(t, seed)
			edit(&st)
			if err := validateConflictResolutionEvidence(&st, st.CompositionConflicts[0], st.CompositionConflicts[0].ResultIDs); err == nil {
				t.Fatal("stale or malformed evidence accepted")
			}
			if err := flowGuard(&st, queued); err == nil {
				t.Fatal("queued parent retained stale authority")
			}
			child := domain.ExternalOperation{ParentID: queued.ID, EnvironmentID: queued.EnvironmentID}
			if err := flowChildAllowed(&st, child); err == nil {
				t.Fatal("queued child retained stale authority")
			}
			store := &memoryStore{state: st}
			before, _ := json.Marshal(store.state)
			if _, err := New(store).Execute(context.Background(), conflictEvidenceReconcile(st)); err == nil {
				t.Fatal("reconcile admitted stale evidence")
			}
			after, _ := json.Marshal(store.state)
			if string(before) != string(after) {
				t.Fatal("rejected reconcile mutated state/events/environment")
			}
		})
	}
}

func TestConflictEvidenceWorkerStopsBeforeProviderEffects(t *testing.T) {
	seed, _ := resolvedConflictEvidenceFixture(t, false)
	m := &memoryStore{state: seed}
	_, _, provider := flowFixture(t)
	svc := NewWithProvider(m, provider)
	queued := exec(t, svc, conflictEvidenceReconcile(seed)).(domain.ExternalOperation)
	exec(t, svc, domain.Command{Action: "record_check_result", CheckID: "caller-check0", Data: map[string]any{"result": "failed", "commit": seed.CompositionConflicts[0].ResolutionCommit}})
	worked, err := svc.Tick(context.Background())
	if err != nil || !worked {
		t.Fatalf("worker did not process queued operation: %v %v", worked, err)
	}
	op := operationByID(&m.state, queued.ID)
	if !terminalOperation(op.Status) || op.DeploymentState != "BLOCKED" || len(provider.plans) != 0 || len(provider.builds) != 0 || len(provider.applyCounts) != 0 {
		t.Fatalf("stale work reached provider or failed to stop: %+v", op)
	}
}

func TestConflictEvidenceMutationAdmissionRechecksCurrentState(t *testing.T) {
	seed, _ := resolvedConflictEvidenceFixture(t, false)
	m := &memoryStore{state: seed}
	queued := exec(t, New(m), conflictEvidenceReconcile(seed)).(domain.ExternalOperation)
	current := operationByID(&m.state, queued.ID)
	current.LeaseOwner = "effect-owner"
	current.LeaseUntil = time.Now().Add(time.Minute)
	queued = cloneFlowOperation(*current)
	ready := cloneCallerState(t, m.state)
	positive := &memoryStore{state: ready}
	if err := New(positive).beginReservationMutation(context.Background(), queued, "effect-owner", "SOURCE"); err != nil {
		t.Fatal("fresh effect admission control", err)
	}
	if operationByID(&positive.state, queued.ID).ReservationMutationIntent != "SOURCE" {
		t.Fatal("fresh mutation intent missing")
	}
	exec(t, New(m), domain.Command{Action: "record_check_result", CheckID: "caller-check0", Data: map[string]any{"result": "failed", "commit": seed.CompositionConflicts[0].ResolutionCommit}})
	before, _ := json.Marshal(m.state)
	if err := New(m).beginReservationMutation(context.Background(), queued, "effect-owner", "SOURCE"); err == nil {
		t.Fatal("effect admission trusted earlier queued evidence")
	}
	after, _ := json.Marshal(m.state)
	if string(before) != string(after) {
		t.Fatal("rejected effect admission changed mutation intent/history")
	}
}

func TestConflictEvidenceSameFeatureNeedsBothIntegrations(t *testing.T) {
	st, _ := resolvedConflictEvidenceFixture(t, true)
	c := st.CompositionConflicts[0]
	if err := validateConflictResolutionEvidence(&st, c, c.ResultIDs); err != nil {
		t.Fatal("same-feature two-sided positive control", err)
	}
	if err := validateConflictResolutionEvidence(&st, c, c.ResultIDs[:1]); err == nil {
		t.Fatal("one integration's evidence stood in for another in same feature")
	}
}

func TestConflictEvidenceAtomicAttachmentAndQueuedBinding(t *testing.T) {
	st, original := resolvedConflictEvidenceFixture(t, false)
	op := operationByID(&st, original)
	sources := append([]domain.FlowSource{}, op.Sources...)
	before, _ := json.Marshal(sources)
	if err := attachConflictResolutions(&st, *op.CompositionSnapshot, sources, []string{st.CompositionConflicts[0].ID, "missing"}); err == nil {
		t.Fatal("invalid later conflict accepted")
	}
	after, _ := json.Marshal(sources)
	if string(before) != string(after) {
		t.Fatal("rejected attachment partially changed caller sources")
	}
	m := &memoryStore{state: st}
	queued := exec(t, New(m), conflictEvidenceReconcile(st)).(domain.ExternalOperation)
	st = m.state
	for _, edit := range []func(*domain.ExternalOperation){
		func(o *domain.ExternalOperation) { o.Sources[0].Plan.ResolutionSHA = "" },
		func(o *domain.ExternalOperation) { o.Sources[0].Plan.ResolutionConflictID = "" },
		func(o *domain.ExternalOperation) { o.Sources[0].Plan.ResolutionSHA = strings.Repeat("d", 40) },
		func(o *domain.ExternalOperation) { o.Sources[0].Plan.BaseSHA = strings.Repeat("d", 40) },
		func(o *domain.ExternalOperation) { o.Sources[0].Plan.HeadSHAs = []string{shaHead} },
		func(o *domain.ExternalOperation) { o.Sources[0].RepositoryID = "other" },
		func(o *domain.ExternalOperation) { o.CompositionSnapshot.ID = "other" },
		func(o *domain.ExternalOperation) { o.ProductID = "other" },
	} {
		candidate := cloneFlowOperation(queued)
		edit(&candidate)
		if err := flowGuard(&st, candidate); err == nil {
			t.Fatal("queued resolution binding changed without rejection")
		}
	}
}

func TestConflictEvidenceReverifySameCommitAcrossPersistentReopen(t *testing.T) {
	seed, _ := resolvedConflictEvidenceFixture(t, false)
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			store, reopen := openWorkerTestStore(t, kind)
			if err := store.Update(context.Background(), func(st *domain.State) error { *st = cloneCallerState(t, seed); return nil }); err != nil {
				t.Fatal(err)
			}
			store.Close()
			store = reopen()
			defer func() { store.Close() }()
			svc := New(store)
			old, _ := store.Read(context.Background())
			conflict := old.CompositionConflicts[0]
			exec(t, svc, domain.Command{Action: "record_check_result", CheckID: "caller-check0", Data: map[string]any{"result": "failed", "commit": conflict.ResolutionCommit}})
			before, _ := store.Read(context.Background())
			if _, err := svc.Execute(context.Background(), conflictEvidenceCommand("verify_conflict_resolution", conflict.ID, map[string]any{"result_ids": conflict.ResultIDs})); err == nil {
				t.Fatal("reverify accepted stale IDs")
			}
			after, _ := store.Read(context.Background())
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejected reverification changed persistent history")
			}
			newResult := exec(t, svc, domain.Command{Action: "record_check_result", CheckID: "caller-check0", Data: map[string]any{"result": "passed", "commit": conflict.ResolutionCommit}}).(domain.CheckResult)
			ids := []string{newResult.ID, conflict.ResultIDs[1]}
			exec(t, svc, conflictEvidenceCommand("verify_conflict_resolution", conflict.ID, map[string]any{"result_ids": ids}))
			store.Close()
			store = reopen()
			svc = New(store)
			current, _ := store.Read(context.Background())
			updated := current.CompositionConflicts[0]
			if updated.ResolutionCommit != conflict.ResolutionCommit || updated.Rationale != conflict.Rationale || updated.ClaimedBy != conflict.ClaimedBy || !reflect.DeepEqual(updated.Source, conflict.Source) || !reflect.DeepEqual(updated.ResultIDs, ids) || len(current.Operations) != len(old.Operations) || len(current.Events) != len(old.Events)+3 {
				t.Fatal("reverification changed source/claim or implicitly queued work")
			}
			if !reflect.DeepEqual(current.Results[:len(old.Results)], old.Results) || len(current.Results) != len(old.Results)+2 {
				t.Fatal("append-only check history changed")
			}
			queued := exec(t, svc, conflictEvidenceReconcile(current)).(domain.ExternalOperation)
			if queued.Sources[0].Plan.ResolutionSHA != conflict.ResolutionCommit || queued.Sources[0].Plan.ResolutionConflictID != conflict.ID {
				t.Fatal("explicit reconcile lost resolution provenance")
			}
		})
	}
}
