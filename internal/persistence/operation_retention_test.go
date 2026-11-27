package persistence

import (
	"context"
	"path/filepath"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"testing"
	"time"
)

func TestOperationRetentionStores(t *testing.T) {
	for _, backend := range []string{"local", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store interface {
				Update(context.Context, func(*domain.State) error) error
				Read(context.Context) (domain.State, error)
			}
			var pg *Store
			if backend == "postgres" {
				pg = retrievalPostgres(t)
				store = pg
			} else {
				local, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer local.Close()
				store = local
			}
			now := time.Now().UTC()
			old := now.Add(-1000 * time.Hour)
			if err := store.Update(ctx, func(st *domain.State) error {
				st.Operations = []domain.ExternalOperation{
					{Meta: domain.Meta{ID: "old", CreatedAt: old, UpdatedAt: old}, Kind: "REFRESH_GIT", Status: "FAILED"},
					{Meta: domain.Meta{ID: "latest", CreatedAt: now, UpdatedAt: now}, Kind: "REFRESH_GIT", Status: "FAILED"},
				}
				st.OperationSteps = []domain.OperationStep{{Meta: domain.Meta{ID: "old-step"}, OperationID: "old", Status: "FAILED"}, {Meta: domain.Meta{ID: "latest-step"}, OperationID: "latest", Status: "FAILED"}}
				st.Events = []domain.Event{{ID: "journal", EntityID: "old", Action: "history"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := application.New(store).PruneOperations(ctx, now, 720*time.Hour, 168*time.Hour); err != nil {
				t.Fatal(err)
			}
			got, err := store.Read(ctx)
			if err != nil || len(got.Operations) != 1 || got.Operations[0].ID != "latest" || len(got.OperationSteps) != 1 || got.OperationSteps[0].ID != "latest-step" || len(got.Events) != 2 || got.Events[0].ID != "journal" {
				t.Fatalf("retention: %+v %v", got, err)
			}
			if pg != nil {
				var count int
				if err := pg.pool.QueryRow(ctx, "SELECT count(*) FROM records WHERE id=ANY($1::text[])", []string{"old", "old-step"}).Scan(&count); err != nil || count != 0 {
					t.Fatalf("SQL records retained: %d %v", count, err)
				}
				if _, ok := pg.cached.positions["old"]; ok {
					t.Fatal("deleted position retained")
				}
				pg.cached = nil // Force a fresh database reload, not just the published cache.
				got, err = pg.Read(ctx)
				if err != nil || len(got.Operations) != 1 || len(got.OperationSteps) != 1 {
					t.Fatalf("reload: %v", err)
				}
			}
		})
	}
}
