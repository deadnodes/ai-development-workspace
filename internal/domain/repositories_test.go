package domain

import "testing"

func TestRepositorySchemaRejectsProvisionalRecords(t *testing.T) {
	for _, role := range []string{"", "SOURCE", "custom"} {
		st := EmptyState()
		st.Repositories = append(st.Repositories, Repository{Role: role})
		if ValidateRepositoryKinds(st) == nil {
			t.Fatalf("accepted repository role %q", role)
		}
	}
	st := EmptyState()
	st.Applications = append(st.Applications, Application{})
	if ValidateRepositoryKinds(st) == nil {
		t.Fatal("accepted missing persisted component kind")
	}
}
