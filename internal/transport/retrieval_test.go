package transport_test

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
	"strings"
	"testing"
)

func TestBoundedContextRetrievalMCPAndHTTP(t *testing.T) {
	ctx := context.Background()
	store, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	st := domain.EmptyState()
	st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "Product"}, {Meta: domain.Meta{ID: "q"}, Name: "Foreign"}}
	st.Features = []domain.Feature{{Meta: domain.Meta{ID: "f", ProductID: "p"}, Title: "Поиск query", Status: "planned", Context: strings.Repeat("界 matching source ", 10000)}, {Meta: domain.Meta{ID: "foreign", ProductID: "q"}, Title: "FOREIGN_PRIVATE", Status: "planned"}}
	st.Integrations = []domain.Integration{{Meta: domain.Meta{ID: "i", ProductID: "p", FeatureID: "f"}, Title: "Child", Objective: "Graph connection", Status: "planned"}}
	st.Events = []domain.Event{{ID: "audit", ProductID: "p", Data: domain.Command{Data: map[string]any{"body": strings.Repeat("AUDIT_PRIVATE", 100000)}}}}
	if e = store.Update(ctx, func(s *domain.State) error { *s = st; return nil }); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(transport.New(application.New(store), transport.Options{}))
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "retrieval-test", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	for _, tc := range []struct {
		name  string
		args  any
		error bool
	}{
		{"search_context", domain.ContextSearchRequest{ProductID: "p", Query: "поиск", Limit: 20}, false},
		{"get_context_excerpt", domain.ContextExcerptRequest{ProductID: "p", ID: "f", Offset: 1, MaxChars: 20}, false},
		{"graph_neighbors", domain.GraphNeighborsRequest{ProductID: "p", ID: "f", Depth: 2, Limit: 30}, false},
		{"search_context", domain.ContextSearchRequest{ProductID: "p", Query: "query", Limit: 21}, true},
		{"get_context_excerpt", domain.ContextExcerptRequest{ProductID: "p", ID: "foreign", MaxChars: 20}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if e != nil {
				t.Fatal(e)
			}
			raw, e := json.Marshal(result)
			if e != nil {
				t.Fatal(e)
			}
			if result.IsError != tc.error {
				t.Fatalf("error=%v want%v %s", result.IsError, tc.error, raw)
			}
			if len(raw) > 128<<10 || strings.Contains(string(raw), "AUDIT_PRIVATE") || strings.Contains(string(raw), "FOREIGN_PRIVATE") {
				t.Fatal("retrieval envelope budget/scope violation")
			}
		})
	}
	response, e := http.Get(server.URL + "/api/context/search?product_id=p&query=" + url.QueryEscape("поиск"))
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(raw), `"id":"f"`) || len(raw) > 64000 {
		t.Fatalf("HTTP search %d %s", response.StatusCode, raw)
	}
	for _, path := range []string{"/api/context/graph?product_id=p&id=f&depth=bad", "/api/context/excerpt?product_id=p&id=f&max_chars=8001"} {
		response, e = http.Get(server.URL + path)
		if e != nil {
			t.Fatal(e)
		}
		response.Body.Close()
		if response.StatusCode < 400 {
			t.Fatal("bad bounds accepted")
		}
	}
}
