package application

import (
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
