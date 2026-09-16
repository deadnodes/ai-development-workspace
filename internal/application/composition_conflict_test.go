package application

import (
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

func TestConflictClaimAndExactTwoSidedVerification(t *testing.T) {
	st := domain.EmptyState()
	st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p"}})
	st.CompositionConflicts = append(st.CompositionConflicts, domain.CompositionConflict{Meta: domain.Meta{ID: "conflict", ProductID: "p"}, Status: "requires_resolution", Integrations: []domain.Integration{{Meta: domain.Meta{ID: "ia", FeatureID: "a"}}, {Meta: domain.Meta{ID: "ib", FeatureID: "b"}}}})
	run := func(action, actor string, data map[string]any) error {
		data["conflict_id"] = "conflict"
		_, e := apply(&st, domain.Command{Action: action, Actor: actor, ProductID: "p", Data: data})
		return e
	}
	if err := run("claim_composition_conflict", "agent/a", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := run("claim_composition_conflict", "agent/b", map[string]any{}); err == nil {
		t.Fatal("stole claim")
	}
	sha := strings.Repeat("a", 40)
	if err := run("record_conflict_resolution", "agent/b", map[string]any{"commit": sha, "rationale": "both contracts preserved"}); err == nil {
		t.Fatal("non-owner resolved")
	}
	if err := run("record_conflict_resolution", "agent/a", map[string]any{"commit": sha, "rationale": "both contracts preserved"}); err != nil {
		t.Fatal(err)
	}
	st.Gates = []domain.Gate{{Meta: domain.Meta{ID: "ga", ProductID: "p"}, IntegrationIDs: []string{"ia"}, Blocking: true}, {Meta: domain.Meta{ID: "gb", ProductID: "p"}, IntegrationIDs: []string{"ib"}, Blocking: true}}
	st.Checks = []domain.Check{{Meta: domain.Meta{ID: "ca", ProductID: "p"}, GateID: "ga"}, {Meta: domain.Meta{ID: "cb", ProductID: "p"}, GateID: "gb"}}
	st.Results = []domain.CheckResult{{Meta: domain.Meta{ID: "r1", ProductID: "p", FeatureID: "a"}, CheckID: "ca", Result: "passed", Commit: sha}, {Meta: domain.Meta{ID: "r2", ProductID: "p", FeatureID: "b"}, CheckID: "cb", Result: "passed", Commit: strings.Repeat("b", 40)}}
	if err := run("verify_conflict_resolution", "tester", map[string]any{"result_ids": []string{"r1"}}); err == nil {
		t.Fatal("missing side verified")
	}
	if err := run("verify_conflict_resolution", "tester", map[string]any{"result_ids": []string{"r1", "r2"}}); err == nil {
		t.Fatal("stale commit verified")
	}
	st.Results[1].Commit = sha
	if err := run("verify_conflict_resolution", "tester", map[string]any{"result_ids": []string{"r1", "r2"}}); err != nil {
		t.Fatal(err)
	}
	if st.CompositionConflicts[0].Status != "resolved" {
		t.Fatal("not resolved")
	}
	if len(st.Events) != 3 {
		t.Fatalf("audit length %d", len(st.Events))
	}
}

func TestConflictContextIncludesBothSidesOnlyForAffectedWork(t *testing.T) {
	st := domain.EmptyState()
	st.CompositionConflicts = []domain.CompositionConflict{{Meta: domain.Meta{ProductID: "p"}, Integrations: []domain.Integration{{Meta: domain.Meta{ID: "a", FeatureID: "fa"}}, {Meta: domain.Meta{ID: "b", FeatureID: "fb"}}}}}
	out := map[string]any{}
	addDeliveryContext(out, st, "fa", "a")
	conflicts := out["composition_conflicts"].([]domain.CompositionConflict)
	if len(conflicts) != 1 || len(conflicts[0].Integrations) != 2 {
		t.Fatal("missing semantic side")
	}
	out = map[string]any{}
	addDeliveryContext(out, st, "unrelated", "")
	if len(out["composition_conflicts"].([]domain.CompositionConflict)) != 0 {
		t.Fatal("unrelated context leaked")
	}
}

func TestCompositionFailurePersistsSemanticConflict(t *testing.T) {
	s, m, f := flowFixture(t)
	f.conflict = true
	flowPlan(t, s, "conflicting", "dev", "rev")
	op := flowStart(t, s, m, "conflicting")
	flowTickUntil(t, s, m, op, func(o domain.ExternalOperation) bool { return terminalOperation(o.Status) })
	if len(m.state.CompositionConflicts) != 1 {
		t.Fatal("no durable conflict")
	}
	c := m.state.CompositionConflicts[0]
	if c.OperationID != op || c.Status != "requires_resolution" || len(c.Integrations) != 1 || len(c.Features) != 1 || c.Source.Result == nil || !c.Source.Result.Conflict {
		t.Fatalf("missing context %+v", c)
	}
}

func TestResolutionCanOnlyRebuildExactPinnedComposition(t *testing.T) {
	st := domain.EmptyState()
	source := domain.FlowSource{RepositoryID: "repo", Plan: delivery.SourcePlan{BaseSHA: shaBase, HeadSHAs: []string{shaHead}}}
	st.CompositionConflicts = []domain.CompositionConflict{{Meta: domain.Meta{ID: "c", ProductID: "p"}, RepositoryID: "repo", CompositionID: "composition", Source: source, Status: "resolved", ResolutionCommit: strings.Repeat("c", 40)}}
	sources := []domain.FlowSource{source}
	comp := domain.Composition{Meta: domain.Meta{ID: "composition", ProductID: "p"}}
	if e := attachConflictResolutions(&st, comp, sources, []string{"c"}); e != nil {
		t.Fatal(e)
	}
	if sources[0].Plan.ResolutionConflictID != "c" || sources[0].Plan.ResolutionSHA != strings.Repeat("c", 40) {
		t.Fatal("missing resolution provenance")
	}
	sources[0].Plan.HeadSHAs = []string{strings.Repeat("d", 40)}
	if e := attachConflictResolutions(&st, comp, sources, []string{"c"}); e == nil {
		t.Fatal("resolution reused for different work")
	}
}
