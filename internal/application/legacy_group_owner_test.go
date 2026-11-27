package application

import (
	"context"
	"testing"

	"releasecontrol/internal/domain"
)

// Groups created before ownership was stored have an empty Owner. The first
// owner-scoped action by the human whose work the group holds (or unassigned
// work) adopts the group; another user's agent cannot capture it.
func TestLegacyGroupWithoutOwnerIsAdoptedByItsHuman(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.run(work.owner, "create_integration", "p", "f", "", "mine", map[string]any{"title": "Mine", "objective": "Ship"})
	work.run(work.owner, "assign_integration", "p", "", "mine", "", map[string]any{"assignee": "alice"})
	if err := work.store.Update(context.Background(), func(st *domain.State) error {
		st.ExecutionGroups = append(st.ExecutionGroups, domain.ExecutionGroup{ID: "legacy", ProductID: "p", Name: "legacy", OrchestratorID: "orch"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := work.apply(work.bobAgent, "add_execution_group_member", "p", "", "", "", map[string]any{"group_id": "legacy", "integration_id": "mine"})
	expectWorkError(t, err, "NOT_ASSIGNEE")
	if g := work.store.state.ExecutionGroups[len(work.store.state.ExecutionGroups)-1]; g.Owner != "" {
		t.Fatalf("another user's agent adopted the legacy group: %q", g.Owner)
	}
	work.run(work.agent, "add_execution_group_member", "p", "", "", "", map[string]any{"group_id": "legacy", "integration_id": "mine"})
	g := work.store.state.ExecutionGroups[len(work.store.state.ExecutionGroups)-1]
	if g.Owner != "alice" || len(g.MemberIntegrationIDs) != 1 {
		t.Fatalf("legacy group not adopted by its human: %+v", g)
	}
}
