package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"path/filepath"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type retrievalTestStore interface {
	Update(context.Context, func(*domain.State) error) error
	SearchContext(context.Context, domain.ContextSearchRequest) (domain.ContextSearchResult, error)
	ContextExcerpt(context.Context, domain.ContextExcerptRequest) (domain.ContextExcerpt, error)
	GraphNeighbors(context.Context, domain.GraphNeighborsRequest) (domain.GraphNeighborsResult, error)
}

func retrievalFixture() domain.State {
	st := domain.EmptyState()
	st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "Product"}, {Meta: domain.Meta{ID: "q"}, Name: "Other"}}
	st.Features = []domain.Feature{{Meta: domain.Meta{ID: "f", ProductID: "p"}, Title: "Быстрый поиск фичей", Context: strings.Repeat("Память界 ", 3000), Status: "planned"}, {Meta: domain.Meta{ID: "foreign", ProductID: "q"}, Title: "FOREIGN поиск", Status: "planned"}}
	st.Integrations = []domain.Integration{{Meta: domain.Meta{ID: "i", ProductID: "p", FeatureID: "f"}, Title: "Indexed search", Objective: "Finding relevant context", Status: "planned", Dependencies: []string{"j", "foreign"}, Repositories: []string{"r"}}, {Meta: domain.Meta{ID: "j", ProductID: "p", FeatureID: "f"}, Title: "Dependency", Objective: "Cycle", Status: "planned", Dependencies: []string{"i"}}}
	st.Memories = []domain.Memory{{Meta: domain.Meta{ID: "m", ProductID: "p", FeatureID: "f"}, IntegrationID: "i", Kind: "progress", Title: "Search receipt", Body: "Cyrillic кириллица lookup"}}
	st.Repositories = []domain.Repository{{Meta: domain.Meta{ID: "r", ProductID: "p"}, Name: "Source repository", Role: "APPLICATION", URL: "https://example.invalid/r", Provider: "git"}}
	st.Events = []domain.Event{{ID: "audit", ProductID: "p", Data: domain.Command{Data: map[string]any{"body": strings.Repeat("AUDIT_ONLY_SECRET", 100000)}}}}
	return st
}
func retrievalPostgres(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for PostgreSQL retrieval tests")
	}
	ctx := context.Background()
	conn, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := fmt.Sprintf("retrieval_%d", time.Now().UnixNano())
	if _, e = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { conn.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); conn.Close(ctx) })
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	values := u.Query()
	values.Set("search_path", schema)
	u.RawQuery = values.Encode()
	s, e := Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	return s
}
func TestContextRetrievalStores(t *testing.T) {
	for _, backend := range []string{"local", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store retrievalTestStore
			if backend == "postgres" {
				store = retrievalPostgres(t)
			} else {
				s, e := OpenLocal(ctx, filepath.Join(t.TempDir(), "memory.db"))
				if e != nil {
					t.Fatal(e)
				}
				defer s.Close()
				store = s
			}
			if e := store.Update(ctx, func(st *domain.State) error { *st = retrievalFixture(); return nil }); e != nil {
				t.Fatal(e)
			}
			result, e := store.SearchContext(ctx, domain.ContextSearchRequest{ProductID: "p", Query: "поиск", Limit: 1})
			if e != nil {
				t.Fatal(e)
			}
			if len(result.Items) != 1 || result.Items[0].ID != "f" || !result.Items[0].Truncated {
				t.Fatalf("bad search: %+v", result)
			}
			if len(result.Items[0].Snippet) > 384*4 || !utf8.ValidString(result.Items[0].Snippet) {
				t.Fatal("snippet not rune bounded")
			}
			for _, query := range []string{"AUDIT_ONLY_SECRET", "FOREIGN"} {
				r, e := store.SearchContext(ctx, domain.ContextSearchRequest{ProductID: "p", Query: query, Limit: 20})
				if e != nil {
					t.Fatal(e)
				}
				if len(r.Items) != 0 {
					t.Fatal("audit/foreign text leaked")
				}
			}
			r, e := store.SearchContext(ctx, domain.ContextSearchRequest{ProductID: "p", FeatureID: "foreign", Query: "поиск", Limit: 20})
			if e != nil || len(r.Items) != 0 {
				t.Fatalf("foreign feature search: %+v %v", r, e)
			}
			excerpt, e := store.ContextExcerpt(ctx, domain.ContextExcerptRequest{ProductID: "p", ID: "f", Offset: 20, MaxChars: 37})
			if e != nil {
				t.Fatal(e)
			}
			if utf8.RuneCountInString(excerpt.Text) != 37 || excerpt.Offset != 20 || excerpt.NextOffset == nil || *excerpt.NextOffset != 57 || !utf8.ValidString(excerpt.Text) {
				t.Fatalf("bad excerpt: %+v", excerpt)
			}
			foreign, e := store.ContextExcerpt(ctx, domain.ContextExcerptRequest{ProductID: "p", ID: "foreign", MaxChars: 100})
			if e != nil || foreign.ID != "" {
				t.Fatal("foreign excerpt visible")
			}
			graph, e := store.GraphNeighbors(ctx, domain.GraphNeighborsRequest{ProductID: "p", ID: "i", Depth: 2, Limit: 30})
			if e != nil {
				t.Fatal(e)
			}
			seen := map[string]bool{"i": true}
			for _, node := range graph.Nodes {
				if node.ProductID != "p" || seen[node.ID] || node.ID == "foreign" {
					t.Fatal("graph scope/cycle violated")
				}
				seen[node.ID] = true
			}
			if !seen["j"] || !seen["f"] || !seen["m"] || !seen["r"] {
				t.Fatalf("typed edges missing: %+v", graph)
			}
			bounded, e := store.GraphNeighbors(ctx, domain.GraphNeighborsRequest{ProductID: "p", ID: "i", Depth: 2, Limit: 1})
			if e != nil || len(bounded.Nodes) != 1 || !bounded.Truncated {
				t.Fatalf("bad graph budget: %+v %v", bounded, e)
			}
			raw, _ := json.Marshal(graph)
			if len(raw) > 64000 || strings.Contains(string(raw), "AUDIT_ONLY_SECRET") {
				t.Fatal("graph payload budget/audit leak")
			}
		})
	}
}
func TestPostgresRetrievalIndexTracksUpdatesWithoutStateRead(t *testing.T) {
	s := retrievalPostgres(t)
	ctx := context.Background()
	if e := s.Update(ctx, func(st *domain.State) error { *st = retrievalFixture(); return nil }); e != nil {
		t.Fatal(e)
	}
	// This malformed unrelated record would make a full domain.State read fail.
	_, e := s.pool.Exec(ctx, `INSERT INTO records(id,kind,product_id,document) VALUES('bad-audit','runtime_observations','p','{"id":7}'::jsonb)`)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.pool.Exec(ctx, `UPDATE records SET document=jsonb_set(document,'{body}','"updatedword123"'::jsonb) WHERE id='m'`)
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.SearchContext(ctx, domain.ContextSearchRequest{ProductID: "p", Query: "updatedword123", Limit: 10})
	if e != nil || len(r.Items) != 1 || r.Items[0].ID != "m" {
		t.Fatalf("update not indexed: %+v %v", r, e)
	}
	_, e = s.pool.Exec(ctx, `UPDATE records SET document=jsonb_set(document,'{dependencies}','[]'::jsonb) WHERE id='i'`)
	if e != nil {
		t.Fatal(e)
	}
	var n int
	if e = s.pool.QueryRow(ctx, `SELECT count(*) FROM retrieval_edges WHERE source_id='i' AND relation='depends_on'`).Scan(&n); e != nil || n != 0 {
		t.Fatal("stale edges after update", n, e)
	}
	if e = s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname='retrieval_documents_search' AND indexdef LIKE '%USING gin%'`).Scan(&n); e != nil || n != 1 {
		t.Fatal("GIN index absent", e)
	}
	_, e = s.pool.Exec(ctx, `DELETE FROM records WHERE id='m'`)
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.SearchContext(ctx, domain.ContextSearchRequest{ProductID: "p", Query: "updatedword123", Limit: 10})
	if e != nil || len(r.Items) != 0 {
		t.Fatal("deleted text retained in index", e)
	}
}

func TestPostgresRetrievalLargeUniqueTextDoesNotRejectSource(t *testing.T) {
	s := retrievalPostgres(t)
	ctx := context.Background()
	var body strings.Builder
	for i := 0; i < 150000; i++ {
		fmt.Fprintf(&body, "unique%07d ", i)
	}
	st := retrievalFixture()
	st.Features[0].Context = body.String() + " beyondprefixmarker"
	if e := s.Update(ctx, func(current *domain.State) error { *current = st; return nil }); e != nil {
		t.Fatal("derived FTS rejected valid source", e)
	}
	r, e := s.SearchContext(ctx, domain.ContextSearchRequest{ProductID: "p", Query: "beyondprefixmarker", Limit: 1})
	if e != nil || len(r.Items) != 1 || r.Items[0].IndexedChars != 32768 {
		t.Fatalf("full substring fallback after indexed prefix: %+v %v", r, e)
	}
	excerpt, e := s.ContextExcerpt(ctx, domain.ContextExcerptRequest{ProductID: "p", ID: "f", Offset: r.Items[0].TextChars - 18, MaxChars: 18})
	if e != nil || !strings.Contains(excerpt.Text, "beyondprefixmarker") {
		t.Fatalf("full source lost %+v %v", excerpt, e)
	}
}
