package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"releasecontrol/internal/domain"
	"testing"
	"time"
)

func TestDocumentIndexPreservesExactMatching(t *testing.T) {
	original := json.RawMessage(`{"id":"run-1","status":"RUNNING"}`)
	index := indexDocuments(map[string][]json.RawMessage{"operations": {original}})
	for _, tc := range []struct {
		kind, raw string
		want      bool
	}{
		{"operations", string(original), true},
		{"operations", `{"id":"run-1","status":"SUCCEEDED"}`, false},
		{"operations", `{"status":"RUNNING","id":"run-1"}`, false},
		{"memories", string(original), false},
	} {
		_, got := index.documents[tc.kind][tc.raw]
		if got != tc.want {
			t.Errorf("exact match %s %s = %v, want %v", tc.kind, tc.raw, got, tc.want)
		}
	}
}

func TestDocumentIndexProductDeletion(t *testing.T) {
	product := json.RawMessage(`{"id":"p"}`)
	feature := json.RawMessage(`{"id":"f","product_id":"p"}`)
	before := map[string][]json.RawMessage{"products": {product}, "features": {feature}}
	tests := []struct {
		name  string
		after map[string][]json.RawMessage
		kind  string
		raw   json.RawMessage
		want  bool
	}{
		{"deleted owned record", nil, "features", feature, true},
		{"deleted product", nil, "products", product, true},
		{"product retained", map[string][]json.RawMessage{"products": {product}}, "features", feature, false},
		{"owned record retained", map[string][]json.RawMessage{"features": {feature}}, "features", feature, false},
		{"other owner reference retained", map[string][]json.RawMessage{"memories": {json.RawMessage(`{"id":"m","product_id":"p"}`)}}, "features", feature, false},
		{"record moved to another owner", map[string][]json.RawMessage{"features": {json.RawMessage(`{"id":"f","product_id":"q"}`)}}, "features", feature, false},
		{"unowned instance record", nil, "operations", json.RawMessage(`{"id":"o"}`), false},
		{"missing former product", nil, "features", json.RawMessage(`{"id":"f","product_id":"unknown"}`), false},
		{"malformed record", nil, "features", json.RawMessage(`{"id":`), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := indexDocuments(tc.after).productRecordRemoved(indexDocuments(before), tc.kind, tc.raw)
			if got != tc.want {
				t.Fatalf("removed = %v, want %v", got, tc.want)
			}
			if legacy := productRecordRemoved(before, tc.after, tc.kind, tc.raw); got != legacy {
				t.Fatalf("indexed deletion = %v, original local-store guard = %v", got, legacy)
			}
		})
	}
}

// The incident had 24,499 retained operation documents. Exercise both indexes
// and both per-record lookups at that size, including one changed operation.
func retainedOperations(n int) map[string][]json.RawMessage {
	rows := make([]json.RawMessage, n)
	for i := range rows {
		rows[i] = json.RawMessage(fmt.Sprintf(`{"id":"operation-%d","product_id":"p","status":"SUCCEEDED"}`, i))
	}
	return map[string][]json.RawMessage{"products": {json.RawMessage(`{"id":"p"}`)}, "operations": rows}
}

func TestDocumentIndexRetainedOperations(t *testing.T) {
	before := retainedOperations(24499)
	after := retainedOperations(24499)
	after["operations"][24498] = json.RawMessage(`{"id":"operation-24498","product_id":"p","status":"FAILED"}`)
	old, current := indexDocuments(before), indexDocuments(after)
	changed := 0
	for _, raw := range after["operations"] {
		if _, found := old.documents["operations"][string(raw)]; !found {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	for _, raw := range before["operations"] {
		if current.productRecordRemoved(old, "operations", raw) {
			t.Fatal("retained operation marked deleted")
		}
	}
}

func BenchmarkDocumentIndexRetainedOperations(b *testing.B) {
	for _, n := range []int{1000, 24499} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			before, after := retainedOperations(n), retainedOperations(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				old, current := indexDocuments(before), indexDocuments(after)
				for _, raw := range after["operations"] {
					if _, ok := old.documents["operations"][string(raw)]; !ok {
						b.Fatal("unchanged record missing")
					}
				}
				for _, raw := range before["operations"] {
					if current.productRecordRemoved(old, "operations", raw) {
						b.Fatal("retained operation deleted")
					}
				}
			}
		})
	}
}

func TestPostgresUpdateRetainsLargeOperationHistory(t *testing.T) {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("set TEST_DATABASE_URL for real PostgreSQL regression")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	schema := fmt.Sprintf("write_scale_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	store, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Update(ctx, func(st *domain.State) error {
		st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p"}, Name: "Retained history"})
		for i := 0; i < 24499; i++ {
			st.Operations = append(st.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: fmt.Sprintf("op-%d", i), ProductID: "p"}, Status: "SUCCEEDED"})
		}
		st.Events = append(st.Events, domain.Event{ID: "seed-event", ProductID: "p", Action: "seed"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = store.Update(bounded, func(st *domain.State) error {
		st.Operations[24498].Status = "FAILED"
		st.Events = append(st.Events, domain.Event{ID: "change-event", ProductID: "p", Action: "change"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("update of 24,499 retained operations: %s", time.Since(started))
	state, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Operations) != 24499 || len(state.Events) != 2 {
		t.Fatalf("history lost: %d operations, %d events", len(state.Operations), len(state.Events))
	}
	changed := 0
	for _, operation := range state.Operations {
		if operation.Status == "FAILED" {
			changed++
			if operation.ID != "op-24498" {
				t.Fatal("wrong operation changed")
			}
		}
	}
	if changed != 1 || state.Events[0].ID != "seed-event" || state.Events[1].ID != "change-event" {
		t.Fatal("change or append-only event order not persisted")
	}
}
