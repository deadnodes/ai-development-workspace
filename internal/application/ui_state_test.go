package application_test

import (
	"context"
	"fmt"
	"path/filepath"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"testing"
)

func TestUIStateFeatureScopeAndHistoryBudget(t *testing.T) {
	ctx := context.Background()
	store, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "ui.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	e = store.Update(ctx, func(s *domain.State) error {
		s.Products = append(s.Products, domain.Product{Meta: domain.Meta{ID: "a"}, Name: "A"}, domain.Product{Meta: domain.Meta{ID: "b"}, Name: "B"})
		s.Features = append(s.Features, domain.Feature{Meta: domain.Meta{ID: "f", ProductID: "b"}, Title: "Feature", Status: "active"})
		for i := 0; i < 120; i++ {
			s.Memories = append(s.Memories, domain.Memory{Meta: domain.Meta{ID: fmtID(i), ProductID: "b", FeatureID: "f"}, Kind: "progress", Body: "evidence"})
		}
		s.RuntimeObservations = append(s.RuntimeObservations, domain.RuntimeObservation{Meta: domain.Meta{ID: "raw", ProductID: "b"}})
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	out, e := application.New(store).UIState(ctx, "a", "f")
	if e != nil {
		t.Fatal(e)
	}
	if out.Projection.ProductID != "b" || len(out.Memories) != 100 || len(out.RuntimeObservations) != 0 || len(out.Features) != 1 || len(out.Products) != 2 {
		t.Fatalf("bad projection: scope=%s memories=%d raw=%d", out.Projection.ProductID, len(out.Memories), len(out.RuntimeObservations))
	}
	if len(out.Projection.Truncated) != 1 || out.Projection.Truncated[0] != "memories" {
		t.Fatal("history truncation not disclosed")
	}
	full, e := store.Read(ctx)
	if e != nil || len(full.Memories) != 120 || len(full.RuntimeObservations) != 1 {
		t.Fatal("projection changed source data")
	}
}
func fmtID(i int) string { return fmt.Sprintf("m%03d", i) }
