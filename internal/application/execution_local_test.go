package application

import (
	"context"
	"path/filepath"
	"testing"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
)

func TestCreateExecutionGroupWithLocalStore(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := New(store)
	run := func(c domain.Command) any {
		t.Helper()
		c.Actor = "agent"
		v, err := s.Execute(ctx, c)
		if err != nil {
			t.Fatalf("%s: %v", c.Action, err)
		}
		return v
	}
	run(domain.Command{Action: "create_product", ID: "p", Data: map[string]any{"name": "P"}})
	run(domain.Command{Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "F", "goal": "G"}})
	for i := 1; i <= 4; i++ {
		run(domain.Command{Action: "create_integration", ID: string(rune('a' + i)), FeatureID: "f", Data: map[string]any{"title": "I", "objective": "O"}})
	}
	run(domain.Command{Action: "transition_integration", IntegrationID: "b", Data: map[string]any{"status": "working"}})
	run(domain.Command{Action: "transition_integration", IntegrationID: "b", Data: map[string]any{"status": "ready"}})
	run(domain.Command{Action: "create_execution_group", ProductID: "p", Data: map[string]any{"name": "G", "orchestrator_id": "orch", "integration_ids": []string{"b", "c", "d", "e"}}})
}
