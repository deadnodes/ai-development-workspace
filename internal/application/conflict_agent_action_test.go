package application

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

func callerConflictFixture(t *testing.T) (domain.State, string) {
	t.Helper()
	s, m, provider := flowFixture(t)
	exec(t, s, domain.Command{Action: "create_feature", ID: "caller-f2", ProductID: "p", Data: map[string]any{"title": "Second feature", "goal": "Preserve B"}})
	exec(t, s, domain.Command{Action: "create_integration", ID: "caller-i2", FeatureID: "caller-f2", Data: map[string]any{"title": "B", "objective": "Keep B"}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "caller-rev2", IntegrationID: "caller-i2", Data: map[string]any{"repository_id": "source", "branch": "feature/second", "base_commit": shaBase, "head_commit": strings.Repeat("e", 40), "commits": []string{strings.Repeat("e", 40)}}})
	for n, side := range []struct{ feature, integration string }{{"f", "i"}, {"caller-f2", "caller-i2"}} {
		exec(t, s, domain.Command{Action: "create_gate", ID: fmt.Sprintf("caller-g%d", n), FeatureID: side.feature, Data: map[string]any{"title": "Acceptance", "reason": "Preserve both features", "integration_ids": []string{side.integration}, "blocking": true}})
		exec(t, s, domain.Command{Action: "add_check", ID: fmt.Sprintf("caller-check%d", n), GateID: fmt.Sprintf("caller-g%d", n), Data: map[string]any{"title": "Check", "mechanism": "automated"}})
	}
	provider.conflict = true
	flowPlan(t, s, "caller-composition", "dev", "rev", "caller-rev2")
	opID := flowStart(t, s, m, "caller-composition")
	flowTickUntil(t, s, m, opID, func(o domain.ExternalOperation) bool { return terminalOperation(o.Status) })
	if len(m.state.CompositionConflicts) != 1 {
		t.Fatal("fixture did not reach a persisted conflict")
	}
	return m.state, opID
}

func cloneCallerState(t *testing.T, st domain.State) domain.State {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var out domain.State
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCallerConflictActionFrozenInputsAndPrivacy(t *testing.T) {
	st, opID := callerConflictFixture(t)
	op := operationByID(&st, opID)
	baseline := operationConflictAction(&st, *op, time.Now().UTC())
	if baseline == nil || baseline.Status != "ACTION_REQUIRED" || baseline.Conflicts[0].Inputs[0].RetainedRef == "" || baseline.Conflicts[0].Inputs[0].ObservedAt == nil {
		t.Fatal("matching stored retention evidence missing")
	}
	// Snapshot order and current branch observations are not composition order.
	slices.Reverse(op.CompositionSnapshot.RevisionSnapshots)
	st.IntegrationRevisions[0].Branch = "feature/moved"
	st.IntegrationRevisions[0].HeadCommit = strings.Repeat("d", 40)
	c := &st.CompositionConflicts[0]
	secret := "DO_NOT_COPY_PROVIDER_OR_HISTORY"
	c.Source.Connection.PrivateKeyRef = secret
	c.Memories = append(c.Memories, domain.Memory{Body: strings.Repeat(secret, 10000)})
	c.Rationale = secret
	c.Source.Result.Detail = secret
	foreign := *c
	foreign.ID, foreign.ProductID = "foreign-conflict", "foreign"
	st.CompositionConflicts = append(st.CompositionConflicts, foreign)
	unrelated := *c
	unrelated.ID, unrelated.OperationID = "other-operation-conflict", "other-operation"
	st.CompositionConflicts = append(st.CompositionConflicts, unrelated)
	before, _ := json.Marshal(st)
	action := operationConflictAction(&st, *op, time.Now().UTC())
	if action == nil || action.Status != "ACTION_REQUIRED" || len(action.Conflicts) != 1 {
		t.Fatalf("missing caller task: %+v", action)
	}
	got := action.Conflicts[0]
	if got.BaseSHA != shaBase || got.BaseRef != "main" || got.Repository != "42" || len(got.Inputs) != 2 || got.Inputs[0].RevisionID != "rev" || got.Inputs[0].OriginBranch != "feature/work" || got.Inputs[0].HeadSHA != shaHead || got.Inputs[1].RevisionID != "caller-rev2" || got.Inputs[1].OriginBranch != "feature/second" || got.Inputs[1].HeadSHA != strings.Repeat("e", 40) {
		t.Fatalf("lost frozen input order/identity: %+v", got)
	}
	if len(got.RequiredChecks) != 2 || got.Inputs[0].RetainedRef != "" || got.Inputs[0].ObservedAt != nil {
		t.Fatalf("missing checks or accepted changed retention authority: %+v", got)
	}
	b, _ := json.Marshal(action)
	if len(b) > conflictAgentActionBytes || strings.Contains(string(b), secret) || strings.Contains(string(b), "foreign-conflict") || strings.Contains(string(b), "other-operation-conflict") || strings.Contains(string(b), "feature/moved") {
		t.Fatalf("unsafe caller projection: %s", b)
	}
	after, _ := json.Marshal(st)
	if string(before) != string(after) {
		t.Fatal("read-only projection changed state")
	}
}

func TestCallerConflictActionRejectsAmbiguousInputs(t *testing.T) {
	seed, opID := callerConflictFixture(t)
	for _, tc := range []struct {
		name string
		edit func(*domain.State, *domain.ExternalOperation, *domain.CompositionConflict)
	}{
		{"wrong-composition", func(_ *domain.State, _ *domain.ExternalOperation, c *domain.CompositionConflict) {
			c.CompositionID = "other"
		}},
		{"wrong-product-revision", func(_ *domain.State, o *domain.ExternalOperation, _ *domain.CompositionConflict) {
			o.CompositionSnapshot.RevisionSnapshots[0].ProductID = "foreign"
		}},
		{"duplicate-revision", func(_ *domain.State, o *domain.ExternalOperation, _ *domain.CompositionConflict) {
			o.CompositionSnapshot.RevisionSnapshots = append(o.CompositionSnapshot.RevisionSnapshots, o.CompositionSnapshot.RevisionSnapshots[0])
		}},
		{"swapped-heads", func(_ *domain.State, o *domain.ExternalOperation, c *domain.CompositionConflict) {
			slices.Reverse(c.Source.Plan.HeadSHAs)
			o.Sources[0].Plan.HeadSHAs = append([]string{}, c.Source.Plan.HeadSHAs...)
		}},
		{"wrong-integration-feature", func(_ *domain.State, _ *domain.ExternalOperation, c *domain.CompositionConflict) {
			c.Integrations[0].FeatureID = "foreign"
		}},
		{"wrong-check-product", func(_ *domain.State, _ *domain.ExternalOperation, c *domain.CompositionConflict) {
			c.Checks[0].ProductID = "foreign"
		}},
		{"wrong-gate-feature", func(_ *domain.State, _ *domain.ExternalOperation, c *domain.CompositionConflict) {
			c.Gates[0].FeatureID = "caller-f2"
		}},
		{"duplicate-integration", func(_ *domain.State, _ *domain.ExternalOperation, c *domain.CompositionConflict) {
			c.Integrations = append(c.Integrations, c.Integrations[0])
		}},
		{"duplicate-conflict", func(st *domain.State, _ *domain.ExternalOperation, c *domain.CompositionConflict) {
			st.CompositionConflicts = append(st.CompositionConflicts, *c)
		}},
		{"missing-source", func(_ *domain.State, o *domain.ExternalOperation, _ *domain.CompositionConflict) { o.Sources = nil }},
		{"missing-source-result", func(_ *domain.State, o *domain.ExternalOperation, _ *domain.CompositionConflict) {
			o.Sources[0].Result = nil
		}},
		{"nonconflicting-source-result", func(_ *domain.State, o *domain.ExternalOperation, _ *domain.CompositionConflict) {
			o.Sources[0].Result.Conflict = false
		}},
		{"missing-record-result", func(_ *domain.State, _ *domain.ExternalOperation, c *domain.CompositionConflict) {
			c.Source.Result = nil
		}},
		{"nonconflicting-record-result", func(_ *domain.State, _ *domain.ExternalOperation, c *domain.CompositionConflict) {
			c.Source.Result.Conflict = false
		}},
		{"another-conflict-without-record", func(_ *domain.State, o *domain.ExternalOperation, _ *domain.CompositionConflict) {
			second := o.Sources[0]
			second.RepositoryID = "second-repo"
			o.Sources = append(o.Sources, second)
		}},
		{"missing-record", func(st *domain.State, _ *domain.ExternalOperation, _ *domain.CompositionConflict) {
			st.CompositionConflicts = nil
		}},
		{"cancelled", func(_ *domain.State, o *domain.ExternalOperation, _ *domain.CompositionConflict) {
			o.Status = "CANCELLED"
		}},
		{"oversized-identity", func(_ *domain.State, _ *domain.ExternalOperation, c *domain.CompositionConflict) {
			c.ID = strings.Repeat("x", 129)
		}},
		{"oversized-branch", func(_ *domain.State, o *domain.ExternalOperation, _ *domain.CompositionConflict) {
			o.CompositionSnapshot.RevisionSnapshots[0].Branch = strings.Repeat("x", 201)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := cloneCallerState(t, seed)
			op := operationByID(&st, opID)
			tc.edit(&st, op, &st.CompositionConflicts[0])
			action := operationConflictAction(&st, *op, time.Now().UTC())
			if action == nil || action.Status != "UNAVAILABLE" || len(action.Conflicts) != 0 || len(action.Workflow) != 0 {
				t.Fatalf("partial/ambiguous action admitted: %+v", action)
			}
		})
	}
}

func TestCallerConflictActionProviderAddressWhitelist(t *testing.T) {
	seed, opID := callerConflictFixture(t)
	for _, address := range []string{"42", "9223372036854775807", "owner/repo", "owner/repo.name"} {
		st := cloneCallerState(t, seed)
		op := operationByID(&st, opID)
		op.Sources[0].Plan.Repository, st.CompositionConflicts[0].Source.Plan.Repository = address, address
		out := operationConflictAction(&st, *op, time.Now().UTC())
		if out == nil || out.Status != "ACTION_REQUIRED" || out.Conflicts[0].Repository != address {
			t.Fatalf("valid provider identity rejected: %q %+v", address, out)
		}
	}
	for _, address := range []string{"https://operator:DO_NOT_COPY@github.example/owner/repo", "DO_NOT_COPY", "0", "-1", "+42", "042", "9223372036854775808", "owner/repo?token=DO_NOT_COPY"} {
		st := cloneCallerState(t, seed)
		op := operationByID(&st, opID)
		op.Sources[0].Plan.Repository, st.CompositionConflicts[0].Source.Plan.Repository = address, address
		out := operationConflictAction(&st, *op, time.Now().UTC())
		b, _ := json.Marshal(out)
		if out == nil || out.Status != "UNAVAILABLE" || len(out.Conflicts) != 0 || len(out.Workflow) != 0 || strings.Contains(string(b), "DO_NOT_COPY") {
			t.Fatalf("invalid provider identity disclosed: %q %s", address, b)
		}
	}
}

func TestCallerConflictActionStatusesAndRetainedEvidence(t *testing.T) {
	seed, opID := callerConflictFixture(t)
	for _, status := range []string{"requires_resolution", "claimed", "verification_required", "resolved"} {
		t.Run(status, func(t *testing.T) {
			st := cloneCallerState(t, seed)
			c := &st.CompositionConflicts[0]
			c.Status, c.ClaimedBy = status, "another-agent"
			if status == "verification_required" || status == "resolved" {
				c.ResolutionCommit = strings.Repeat("c", 40)
			}
			out := operationConflictAction(&st, *operationByID(&st, opID), time.Now().UTC())
			if out.Status != "ACTION_REQUIRED" || out.Conflicts[0].Status != status || !out.Workflow[len(out.Workflow)-1].MayBuildAndDeploy {
				t.Fatalf("wrong workflow: %+v", out)
			}
			if !strings.Contains(out.Workflow[2].When, "resolved with superseded evidence") || !strings.Contains(out.Workflow[3].When, "same immutable resolution commit") {
				t.Fatal("requesting agent lacks same-commit reverification recovery instructions")
			}
			b, _ := json.Marshal(out)
			if strings.Contains(string(b), "another-agent") {
				t.Fatal("actor/ownership asserted by a query with no caller identity")
			}
		})
	}
	for _, edit := range []func(*domain.CompositionConflict){
		func(c *domain.CompositionConflict) { c.Source.Retention = nil },
		func(c *domain.CompositionConflict) { c.Source.Retained = nil },
		func(c *domain.CompositionConflict) { c.Source.Retained[0].IntentHash = "wrong" },
		func(c *domain.CompositionConflict) { c.Source.Retained[0].ObservedAt = time.Now().Add(time.Hour) },
	} {
		st := cloneCallerState(t, seed)
		edit(&st.CompositionConflicts[0])
		out := operationConflictAction(&st, *operationByID(&st, opID), time.Now().UTC())
		if out.Status != "ACTION_REQUIRED" || out.Conflicts[0].Inputs[0].RetainedRef != "" || out.Conflicts[0].Inputs[0].ObservedAt != nil {
			t.Fatalf("unproven optional ref: %+v", out)
		}
	}
	st := cloneCallerState(t, seed)
	if out := operationConflictAction(&st, domain.ExternalOperation{Meta: domain.Meta{ID: "no-conflict", ProductID: "p"}}, time.Now().UTC()); out != nil {
		t.Fatal("invented agent work for unrelated operation")
	}
}

func TestCallerConflictActionByteBudget(t *testing.T) {
	st, opID := callerConflictFixture(t)
	c := &st.CompositionConflicts[0]
	gate := c.Gates[0]
	gate.ID = strings.Repeat("g", 128)
	c.Gates, c.Checks = []domain.Gate{gate}, nil
	for n := range 128 {
		c.Checks = append(c.Checks, domain.Check{Meta: domain.Meta{ID: fmt.Sprintf("%0128d", n), ProductID: "p", FeatureID: gate.FeatureID}, GateID: gate.ID})
	}
	out := operationConflictAction(&st, *operationByID(&st, opID), time.Now().UTC())
	b, _ := json.Marshal(out)
	if out.Status != "UNAVAILABLE" || out.Reason != "CONFLICT_CONTEXT_TOO_LARGE" || len(out.Conflicts) != 0 || len(b) > conflictAgentActionBytes {
		t.Fatalf("oversized/partial handoff: %s", b)
	}
}

func TestCallerConflictActionStoredQueryAndReopen(t *testing.T) {
	seed, opID := callerConflictFixture(t)
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			store, reopen := openWorkerTestStore(t, kind)
			if err := store.Update(context.Background(), func(st *domain.State) error { *st = cloneCallerState(t, seed); return nil }); err != nil {
				store.Close()
				t.Fatal(err)
			}
			store.Close()
			store = reopen()
			defer store.Close()
			before, _ := store.Read(context.Background())
			svc := New(store)
			v, err := svc.Query(context.Background(), "get_operation", opID)
			if err != nil {
				t.Fatal(err)
			}
			out := v.(map[string]any)
			action, ok := out["agent_action"].(*conflictAgentAction)
			if !ok || action.Status != "ACTION_REQUIRED" || len(action.Conflicts[0].Inputs) != 2 || out["operation"] == nil || out["steps"] == nil || out["children"] == nil || out["feature_contexts"] == nil {
				t.Fatalf("missing persisted/query handoff: %+v", out)
			}
			after, _ := store.Read(context.Background())
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			if string(a) != string(b) {
				t.Fatal("get_operation mutated persistent state")
			}
		})
	}
}
