package application

import (
	"reflect"
	"releasecontrol/internal/domain"
	"testing"
)

func TestDeleteProductIsolationAndConfirmation(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "create_product", ID: "other", Data: map[string]any{"name": "Other"}})
	reject(t, s, domain.Command{Action: "delete_product", ProductID: "p", Data: map[string]any{"name": "wrong"}})
	m.state.Operations = append(m.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "op", ProductID: "p"}, Status: "RUNNING"})
	reject(t, s, domain.Command{Action: "delete_product", ProductID: "p", Data: map[string]any{"name": "Product"}})
	m.state.Operations[0].Status = "FAILED"
	m.state.RegisteredRepositories = append(m.state.RegisteredRepositories, domain.RegisteredRepository{Meta: domain.Meta{ID: "shared"}})
	before := len(m.state.Events)
	exec(t, s, domain.Command{Action: "delete_product", ProductID: "p", Data: map[string]any{"name": "Product"}})
	if len(m.state.Products) != 1 || m.state.Products[0].ID != "other" || len(m.state.Features) != 0 || len(m.state.Integrations) != 0 || len(m.state.Operations) != 0 || len(m.state.RegisteredRepositories) != 1 || len(m.state.Events) != before+1 {
		t.Fatal("incorrect deletion scope")
	}
}

func TestDeleteProductPreservesOptionalFieldsOfOtherProducts(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "create_product", ID: "other", Data: map[string]any{"name": "Other"}})
	m.state.Memories = []domain.Memory{
		{Meta: domain.Meta{ID: "removed", ProductID: "p"}, Kind: "blocker", Status: "resolved", Resolution: "removed resolution"},
		{Meta: domain.Meta{ID: "kept", ProductID: "other"}, Kind: "decision", Reason: "Keep unchanged"},
	}
	expected := m.state.Memories[1]
	exec(t, s, domain.Command{Action: "delete_product", ProductID: "p", Data: map[string]any{"name": "Product"}})
	if len(m.state.Memories) != 1 || !reflect.DeepEqual(expected, m.state.Memories[0]) {
		t.Fatalf("surviving record changed: %+v", m.state.Memories)
	}
	if err := domain.ValidateStateStatuses(m.state); err != nil {
		t.Fatal(err)
	}
}
