package persistence

import (
	"context"
	"encoding/json"
	"testing"

	"releasecontrol/internal/domain"
)

func TestPrivateLocalReferenceClosureScansDocumentOnce(t *testing.T) {
	data := []byte(`{"operations":[{"id":"root","product_id":"p","parent_id":"parent"},{"id":"parent","product_id":"p","environment_id":"env"}],"environments":[{"id":"env","product_id":"p"}],"products":[{"id":"p"}]}`)
	groups := map[string][]json.RawMessage{}
	var scans int
	err := readLocalReferences(context.Background(), data, []string{"root"}, groups, func(doc []byte, visit func(string, []byte) error) error {
		scans++
		return objectFields(doc, visit)
	})
	if err != nil || scans != 1 || len(groups["operations"]) != 2 || len(groups["environments"]) != 1 || len(groups["products"]) != 0 {
		t.Fatalf("scans=%d groups=%v err=%v", scans, groups, err)
	}
}

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
