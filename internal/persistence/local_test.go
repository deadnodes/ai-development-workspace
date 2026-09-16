package persistence_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
)

func TestLocalRestartRollbackAndDomainValidation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "state.db")
	store, err := persistence.OpenLocal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	svc := application.New(store)
	if _, err = svc.Execute(ctx, domain.Command{Action: "create_product", ID: "p", Actor: "agent", Data: map[string]any{"name": "Local product"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Execute(ctx, domain.Command{Action: "create_feature", ID: "f", ProductID: "p", Actor: "agent", Data: map[string]any{"title": "Context", "goal": "Persist"}}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*domain.State) error{
		func(s *domain.State) error {
			s.Features[0].Title = "must rollback"
			return errors.New("callback rejected")
		},
		func(s *domain.State) error { s.Features[0].Status = "invented"; return nil },
		func(s *domain.State) error { s.Events[0].Actor = "tampered"; return nil },
		func(s *domain.State) error { s.Products = nil; return nil },
		func(s *domain.State) error { s.Features = append(s.Features, s.Features[0]); return nil },
	} {
		if err = store.Update(ctx, mutate); err == nil {
			t.Fatal("invalid transaction accepted")
		}
	}
	after, err := store.Read(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed update changed persisted state", err)
	}
	after.Features[0].Title = "caller mutation"
	store.Close()
	store, err = persistence.OpenLocal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := store.Read(ctx)
	if err != nil || !reflect.DeepEqual(before, restored) {
		t.Fatal("restart changed state", err)
	}
	backup, err := application.New(store).CreateBackup(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	destination, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "dest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	if _, err = application.New(destination).RestoreBackup(ctx, "agent", backup.ArchiveBase64, backup.SHA256); err != nil {
		t.Fatal(err)
	}
	moved, err := destination.Read(ctx)
	if err != nil || len(moved.Events) != len(before.Events)+1 || len(moved.Features) != 1 {
		t.Fatal("portable backup lost history", err)
	}
}
func TestLocalConcurrentUpdatesAndCancelledWait(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := application.New(store)
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Execute(ctx, domain.Command{Action: "create_product", ID: fmt.Sprintf("p%d", i), Actor: "agent", Data: map[string]any{"name": fmt.Sprintf("Product %d", i)}})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err := store.Read(ctx)
	if err != nil || len(state.Products) != 24 || len(state.Events) != 24 {
		t.Fatal("concurrent update lost data", err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- store.Update(ctx, func(*domain.State) error { close(entered); <-release; return nil }) }()
	<-entered
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	called := false
	if err := store.Update(cancelled, func(*domain.State) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatal("cancelled writer was executed", err)
	}
	closed := make(chan struct{})
	go func() { store.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close raced outstanding transaction")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-closed
	if _, err := store.Read(ctx); err == nil {
		t.Fatal("read accepted after close")
	}
}
func TestLocalRejectsSecondProcessOwner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := persistence.OpenLocal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	another, err := persistence.OpenLocal(short, path)
	if err == nil {
		another.Close()
		t.Fatal("second owner opened locked local file")
	}
}
