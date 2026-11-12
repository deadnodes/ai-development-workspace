package persistence

import (
	"context"
	"testing"

	"releasecontrol/internal/domain"
)

func TestPostgresReferenceClosureCyclesAndJournalIdentities(t *testing.T) {
	s := retrievalPostgres(t)
	ctx := context.Background()
	if err := s.Update(ctx, func(st *domain.State) error {
		st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "P"}}
		st.Operations = []domain.ExternalOperation{{Meta: domain.Meta{ID: "root", ProductID: "p"}, ParentID: "parent", Status: "SUCCEEDED"}, {Meta: domain.Meta{ID: "parent", ProductID: "p"}, ParentID: "root", EnvironmentID: "env", Status: "SUCCEEDED"}, {Meta: domain.Meta{ID: "audit-ref", ProductID: "p"}, ParentID: "foreign-event", Status: "SUCCEEDED"}}
		st.Environments = []domain.Environment{{Meta: domain.Meta{ID: "env", ProductID: "p"}, Name: "Production metadata"}}
		st.EnvironmentBindings = []domain.EnvironmentBinding{{Meta: domain.Meta{ID: "binding", ProductID: "p"}, EnvironmentID: "env", Purpose: "PROD"}}
		st.Events = []domain.Event{{ID: "foreign-event", ProductID: "other", Action: "record_progress"}, {ID: "unrelated", ProductID: "other", Action: "record_progress"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	refs, err := s.ReadReferences(ctx, []string{"root", "audit-ref"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs.Operations) != 3 || len(refs.Environments) != 1 || len(refs.EnvironmentBindings) != 1 || len(refs.Events) != 1 || refs.Events[0].ID != "foreign-event" || len(refs.Products) != 0 {
		t.Fatalf("incorrect reference closure: %+v", refs)
	}
}
