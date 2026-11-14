package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"releasecontrol/internal/domain"
)

func TestPostgresRevisionCacheIsolationAndSerialization(t *testing.T) {
	a := retrievalPostgres(t)
	ctx := context.Background()
	// Open another independent cache against exactly the same isolated schema.
	b, err := Open(ctx, a.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = a.Update(ctx, func(st *domain.State) error {
		st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p"}, Name: "initial"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first := a.cached
	first.positions["cache-hit-probe"] = -1
	if err = a.Update(ctx, func(st *domain.State) error { st.Products[0].Name = "warm"; return nil }); err != nil {
		t.Fatal(err)
	}
	if a.cached.positions["cache-hit-probe"] != -1 {
		t.Fatal("warm revision unexpectedly reloaded records")
	}
	if a.cached.revision != first.revision+1 {
		t.Fatal("revision not advanced")
	}
	if err = b.Update(ctx, func(st *domain.State) error {
		if st.Products[0].Name != "warm" {
			t.Fatal("other store missed commit")
		}
		st.Products[0].Name = "external"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = a.Update(ctx, func(st *domain.State) error {
		if st.Products[0].Name != "external" {
			t.Fatal("revision miss did not reload")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.cached.positions["cache-hit-probe"]; ok {
		t.Fatal("external revision did not replace cache")
	}
	committed := a.cached
	rejected := errors.New("rollback")
	if err = a.Update(ctx, func(st *domain.State) error { st.Products[0].Name = "failed"; return rejected }); !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	if a.cached != committed || a.cached.state.Products[0].Name != "external" {
		t.Fatal("callback rollback poisoned cache")
	}
	// Force failure after record upserts, during event insertion.
	if err = a.Update(ctx, func(st *domain.State) error {
		st.Events = append(st.Events, domain.Event{ID: "same-event"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	committed = a.cached
	if err = a.Update(ctx, func(st *domain.State) error {
		st.Products[0].Name = "failed SQL"
		st.Events = append(st.Events, domain.Event{ID: "same-event"})
		return nil
	}); err == nil {
		t.Fatal("expected SQL failure")
	}
	if a.cached != committed || a.cached.state.Products[0].Name != "external" {
		t.Fatal("SQL rollback poisoned cache")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s := a
			if n%2 != 0 {
				s = b
			}
			errs <- s.Update(ctx, func(st *domain.State) error {
				st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: fmt.Sprintf("p-%d", n)}, Name: "serialized"})
				return nil
			})
		}(n)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	st, err := a.Read(ctx)
	if err != nil || len(st.Products) != 13 {
		t.Fatal("lost concurrent write", len(st.Products), err)
	}
	st.Products[0].Name = "caller mutation"
	if err = a.Update(ctx, func(st *domain.State) error {
		if st.Products[0].Name == "caller mutation" {
			t.Fatal("read escaped cache")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresWriteProjectionDoesNotLoadOrdinaryJournal(t *testing.T) {
	s := retrievalPostgres(t)
	ctx := context.Background()
	if err := s.Update(ctx, func(st *domain.State) error {
		st.Events = []domain.Event{{ID: "progress", Action: "record_progress"}, {ID: "close", Action: "close_build"}, {ID: "close-deployment", Action: "close_deployment"}, {ID: "restore", Action: "restore_backup"}, {ID: "root", Action: "assemble_environment"}, {ID: "child", Action: "reconcile_composition"}, {ID: "selection", Action: "update_environment_feature"}, {ID: "report", Action: "worker_report"}, {ID: "pin", Action: "pin_artifact"}, {ID: "release", Action: "release_artifact_pin"}, {ID: "malformed-release", Action: "record_progress", Data: domain.Command{Action: "release_artifact_pin"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.cached = nil // Exercise the cold write projection as well as incremental updates.
	if err := s.Update(ctx, func(st *domain.State) error {
		ids := []string{}
		for _, e := range st.Events {
			ids = append(ids, e.ID)
		}
		want := []string{"close", "close-deployment", "restore", "root", "child", "selection", "report", "pin", "release", "malformed-release"}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("projection/order: %v", ids)
		}
		st.Events = append(st.Events, domain.Event{ID: "progress-2", Action: "record_progress"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(s.cached.state.Events) != 10 {
		t.Fatal("ordinary events accumulated in cache")
	}
	full, err := s.Read(ctx)
	if err != nil || len(full.Events) != 12 {
		t.Fatal("full journal lost", err)
	}
	// Record fixture resets must preserve the write-side index, not transplant
	// the complete reader journal into an append-only callback.
	if err = s.Update(ctx, func(st *domain.State) error { events := st.Events; *st = full; st.Events = events; return nil }); err != nil {
		t.Fatal("record reset with preserved index", err)
	}

	// An invalid JSON event document outside the projection proves writes do not
	// decode the journal. Full history reads must still report the corruption.
	if _, err = s.pool.Exec(ctx, `INSERT INTO events(id,document) VALUES('opaque','{"action":"record_progress","at":"invalid-time"}')`); err != nil {
		t.Fatal(err)
	}
	s.cached = nil
	if err = s.Update(ctx, func(st *domain.State) error { return nil }); err != nil {
		t.Fatal("write decoded unrelated journal", err)
	}
	if _, err = s.Read(ctx); err == nil {
		t.Fatal("full history concealed corrupt document")
	}
}

func TestCloneValueNestedIsolation(t *testing.T) {
	original := domain.State{Events: []domain.Event{{Data: domain.Command{Data: map[string]any{"nested": []any{map[string]any{"value": "before"}}}}}}}
	copy := cloneValue(reflect.ValueOf(original)).Interface().(domain.State)
	copy.Events[0].Data.Data["nested"].([]any)[0].(map[string]any)["value"] = "after"
	if reflect.DeepEqual(original, copy) {
		t.Fatal("nested values were shared")
	}
}

func TestPostgresScopedIdentityIncludesUnprojectedEvents(t *testing.T) {
	s := retrievalPostgres(t)
	ctx := context.Background()
	if err := s.Update(ctx, func(st *domain.State) error {
		st.Events = append(st.Events, domain.Event{ID: "occupied", Action: "record_progress"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err := s.UpdateScoped(ctx, domain.StateMutationScope{MutableCollections: []string{"features"}, RequireAbsentIDs: []string{"occupied"}}, func(st *domain.State) error {
		st.Features = append(st.Features, domain.Feature{Meta: domain.Meta{ID: "occupied"}, Status: "active"})
		return nil
	})
	if !errors.Is(err, domain.ErrIdentityExists) {
		t.Fatal("historical event identity was not protected", err)
	}
	if len(s.cached.state.Features) != 0 {
		t.Fatal("rejected identity poisoned cache")
	}
}

func TestOrdinaryDeltaPreservesReservedIdentities(t *testing.T) {
	before := domain.EmptyState()
	before.ArtifactPins = []domain.ArtifactPin{{Meta: domain.Meta{ID: "pin"}}}
	before.RegistryInventories = []domain.RegistryInventory{{Meta: domain.Meta{ID: "inventory"}}}
	before.Events = []domain.Event{{ID: "release", EntityID: "pin", Action: "release_artifact_pin"}}
	for _, id := range []string{"pin", "inventory", "release"} {
		delta := domain.EmptyState()
		delta.Features = []domain.Feature{{Meta: domain.Meta{ID: id}, Status: "active"}}
		if err := validateOrdinaryDelta(before, before, delta, nil); err == nil {
			t.Fatal("reserved record ID admitted", id)
		}
		if err := validateOrdinaryDelta(before, before, domain.EmptyState(), []domain.Event{{ID: id}}); err == nil {
			t.Fatal("reserved event ID admitted", id)
		}
	}
}

func TestPostgresCacheMatchesReloadAfterOmissionsAndPositionChanges(t *testing.T) {
	s := retrievalPostgres(t)
	ctx := context.Background()
	if err := s.Update(ctx, func(st *domain.State) error {
		st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "P"}}
		st.Features = []domain.Feature{{Meta: domain.Meta{ID: "a", ProductID: "p"}, Status: "active"}, {Meta: domain.Meta{ID: "b", ProductID: "p"}, Status: "active"}, {Meta: domain.Meta{ID: "c", ProductID: "p"}, Status: "active"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*domain.State) error{
		func(st *domain.State) error {
			st.Features = []domain.Feature{st.Features[2], st.Features[0]}
			return nil
		},
		func(st *domain.State) error {
			st.Features[2].Title = "changed"
			st.Features = []domain.Feature{st.Features[2], st.Features[1], st.Features[0]}
			return nil
		},
		func(st *domain.State) error { st.Features[0].Goal = "changed again"; return nil },
	}
	for _, mutate := range mutations {
		if err := s.Update(ctx, mutate); err != nil {
			t.Fatal(err)
		}
		cached, err := s.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		s.cached = nil
		reloaded, err := s.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cached, reloaded) {
			t.Fatal("cache differs from stored positions/omissions/empty collections")
		}
	}
}

func TestPostgresCacheDeletionPredicateMatchesLocalReference(t *testing.T) {
	for _, mode := range []string{"delete-all", "retain-owned-feature", "retain-other-reference", "move-record-owner", "retain-product", "unknown-owner", "unowned"} {
		t.Run(mode, func(t *testing.T) {
			s := retrievalPostgres(t)
			ctx := context.Background()
			before := domain.EmptyState()
			before.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "P"}, {Meta: domain.Meta{ID: "q"}, Name: "Q"}}
			before.Features = []domain.Feature{{Meta: domain.Meta{ID: "f", ProductID: "p"}, Status: "active"}}
			if mode == "unknown-owner" {
				before.Features[0].ProductID = "unknown"
			}
			if mode == "unowned" {
				before.Features[0].ProductID = ""
			}
			if err := s.Update(ctx, func(st *domain.State) error { *st = before; return nil }); err != nil {
				t.Fatal(err)
			}
			after := domain.EmptyState()
			after.Products = before.Products[1:]
			switch mode {
			case "retain-owned-feature":
				after.Features = before.Features
			case "retain-other-reference":
				after.Memories = []domain.Memory{{Meta: domain.Meta{ID: "m", ProductID: "p"}, Kind: "progress"}}
			case "move-record-owner":
				after.Features = append([]domain.Feature{}, before.Features...)
				after.Features[0].ProductID = "q"
			case "retain-product":
				after.Products = before.Products
			}
			oldDocs, err := documents(before)
			if err != nil {
				t.Fatal(err)
			}
			newDocs, err := documents(after)
			if err != nil {
				t.Fatal(err)
			}
			// The local predicate is the executable specification for every old row.
			expected := map[string]bool{}
			for kind, rows := range oldDocs {
				for _, raw := range rows {
					var meta struct {
						ID string `json:"id"`
					}
					if err = json.Unmarshal(raw, &meta); err != nil {
						t.Fatal(err)
					}
					expected[meta.ID] = !productRecordRemoved(oldDocs, newDocs, kind, raw)
				}
			}
			if err = s.Update(ctx, func(st *domain.State) error { *st = after; return nil }); err != nil {
				t.Fatal(err)
			}
			for id, want := range expected {
				var got bool
				if err = s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM records WHERE id=$1)", id).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("record %s retained=%v, local predicate=%v", id, got, want)
				}
			}
		})
	}
}

func TestPostgresReplayIdentityStillRejectsOrphanJournalIDs(t *testing.T) {
	s := retrievalPostgres(t)
	ctx := context.Background()
	if err := s.Update(ctx, func(st *domain.State) error {
		st.Products = []domain.Product{{Meta: domain.Meta{ID: "replay"}, Name: "Existing record"}}
		st.Events = []domain.Event{{ID: "replay", Action: "record_progress"}, {ID: "orphan", Action: "record_progress"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"orphan", "replay"} {
		err := s.UpdateScoped(ctx, domain.StateMutationScope{RequireAbsentIDs: []string{id}, AllowExistingRecords: true}, func(st *domain.State) error { return nil })
		if id == "orphan" && !errors.Is(err, domain.ErrIdentityExists) {
			t.Fatal("orphan journal identity admitted", err)
		}
		if id == "replay" && err != nil {
			t.Fatal("application-approved record reuse rejected", err)
		}
	}
}
