package persistence

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"releasecontrol/internal/domain"
)

func TestPostgresViewIncrementalJournalAndIsolation(t *testing.T) {
	a := retrievalPostgres(t)
	ctx := context.Background()
	b, err := Open(ctx, a.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	const count = 3000
	if err = a.Update(ctx, func(st *domain.State) error {
		for i := 0; i < count; i++ {
			st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: fmt.Sprintf("p-%04d", i)}, Name: "original"})
			st.Events = append(st.Events, domain.Event{ID: fmt.Sprintf("e-%04d", i), Action: "record_progress", Data: domain.Command{Data: map[string]any{"nested": map[string]any{"value": "original"}}}})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read, err := a.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.View(ctx, func(st *domain.State) error {
		if !reflect.DeepEqual(read, *st) {
			t.Fatal("Read/View differ")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	decoded := a.decodedEvents
	if decoded != count {
		t.Fatalf("decoded %d, want %d", decoded, count)
	}
	read.Products[0].Name = "mutated"
	read.Events[0].Data.Data["nested"].(map[string]any)["value"] = "mutated"
	if err = a.View(ctx, func(st *domain.State) error {
		if st.Products[0].Name != "original" || st.Events[0].Data.Data["nested"].(map[string]any)["value"] != "original" {
			t.Fatal("Read poisoned View")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = b.Update(ctx, func(st *domain.State) error {
		st.Products[0].Name = "external"
		st.Events = append(st.Events, domain.Event{ID: "external", Action: "record_progress"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = a.View(ctx, func(st *domain.State) error {
		if len(st.Events) != count+1 || st.Events[count].ID != "external" || st.Products[0].Name != "external" {
			t.Fatal("external commit missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if a.decodedEvents != decoded+1 {
		t.Fatalf("redecoded journal: %d -> %d", decoded, a.decodedEvents)
	}
	for i := 0; i < 3; i++ {
		if _, err = a.Read(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if a.decodedEvents != decoded+1 {
		t.Fatal("warm Read decoded history")
	}
	// A rolled-back insert may leave a sequence gap. Rebuild once, then stay warm.
	if _, err = b.pool.Exec(ctx, "SELECT nextval(pg_get_serial_sequence('events','sequence'))"); err != nil {
		t.Fatal(err)
	}
	if err = b.Update(ctx, func(st *domain.State) error {
		st.Events = append(st.Events, domain.Event{ID: "after-gap", Action: "record_progress"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = a.View(ctx, func(st *domain.State) error {
		if len(st.Events) != count+2 {
			t.Fatal("gap lost events")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	decoded = a.decodedEvents
	if err = a.View(ctx, func(*domain.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if a.decodedEvents != decoded {
		t.Fatal("gap caused repeated rebuild")
	}
}

func TestLocalViewReadParity(t *testing.T) {
	ctx := context.Background()
	s, err := OpenLocal(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Update(ctx, func(st *domain.State) error {
		st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p"}, Name: "original"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.View(ctx, func(st *domain.State) error {
		if !reflect.DeepEqual(read, *st) {
			t.Fatal("Read/View differ")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read.Products[0].Name = "mutated"
	if err = s.View(ctx, func(st *domain.State) error {
		if st.Products[0].Name != "original" {
			t.Fatal("Read poisoned View")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
