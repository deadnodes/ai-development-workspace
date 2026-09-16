package transport_test

import (
	"context"
	"path/filepath"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"testing"
)

func TestProductDeletionPersists(t *testing.T) {
	for _, mode := range []string{"local", "postgres"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var store interface {
				Read(context.Context) (domain.State, error)
				Update(context.Context, func(*domain.State) error) error
				Close()
			}
			var err error
			if mode == "local" {
				store, err = persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
			} else {
				store, err = persistence.Open(ctx, isolatedDatabase(t))
			}
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			s := application.New(store)
			for _, c := range []domain.Command{{Action: "create_product", ID: "delete-me", Data: map[string]any{"name": "Temporary"}}, {Action: "create_feature", ProductID: "delete-me", Data: map[string]any{"title": "History", "goal": "Persist"}}, {Action: "delete_product", ProductID: "delete-me", Data: map[string]any{"name": "Temporary"}}} {
				c.Actor = "test"
				if _, err = s.Execute(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
			st, err := store.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Products) != 0 || len(st.Features) != 0 || len(st.Events) != 3 {
				t.Fatal("deletion not persisted or audit lost")
			}
		})
	}
}
