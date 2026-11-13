package persistence

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
)

func TestRestoreBackupRejectsAuditOnlyInstance(t *testing.T) {
	for _, backend := range []string{"postgres", "bbolt"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store application.Store
			if backend == "postgres" {
				store = retrievalPostgres(t)
			} else {
				local, err := OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { local.Close() })
				store = local
			}
			svc := application.New(store)
			// Capture a valid nonempty backup before deleting the last product.
			if _, err := svc.Execute(ctx, domain.Command{Action: "create_product", ID: "p", Actor: "agent", Data: map[string]any{"name": "Product"}}); err != nil {
				t.Fatal(err)
			}
			backup, err := svc.CreateBackup(ctx, "agent")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.Execute(ctx, domain.Command{Action: "delete_product", ProductID: "p", Actor: "agent", Data: map[string]any{"name": "Product"}}); err != nil {
				t.Fatal(err)
			}
			before, err := store.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(before.Events) == 0 {
				t.Fatal("expected retained audit history")
			}
			records := reflect.ValueOf(before)
			for i := 0; i < records.NumField(); i++ {
				if records.Type().Field(i).Name != "Events" && records.Field(i).Len() != 0 {
					t.Fatalf("expected no records, found %s", records.Type().Field(i).Name)
				}
			}
			for _, event := range before.Events {
				if domain.IsWriteProvenanceEvent(event) {
					t.Fatal("fixture includes write provenance")
				}
			}
			// Refuse with both a warm cache and no cached projection.
			for attempt := 0; attempt < 2; attempt++ {
				if pg, ok := store.(*Store); ok && attempt == 1 {
					pg.cached = nil
				}
				_, err = svc.RestoreBackup(ctx, "agent", backup.ArchiveBase64, backup.SHA256)
				if !errors.Is(err, application.ErrValidation) || !strings.Contains(err.Error(), "restore requires an empty instance") {
					t.Fatalf("expected empty-instance validation refusal, got %v", err)
				}
				after, readErr := store.Read(ctx)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("refused restore changed records or audit history")
				}
			}
		})
	}
}
