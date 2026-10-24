package transport_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

// Exercise the actual SDK envelope, both production stores, and an audit larger
// than an agent should ever receive merely to find a Feature.
func TestBoundedStateNavigationMCP(t *testing.T) {
	for _, backend := range []string{"local", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store application.Store
			if backend == "local" {
				s, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				store = s
			} else {
				s, err := persistence.Open(ctx, isolatedDatabase(t))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				store = s
			}
			state := domain.EmptyState()
			state.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "Product"}, {Meta: domain.Meta{ID: "q"}, Name: "Other"}}
			for i := 0; i < 120; i++ {
				state.Features = append(state.Features, domain.Feature{Meta: domain.Meta{ID: fmt.Sprintf("f%03d", i), ProductID: "p"}, Title: strings.Repeat("界<&\n", 10000), Status: "planned", Context: "PRIVATE_CONTEXT" + strings.Repeat("c", 1<<14)})
			}
			state.Features = append(state.Features, domain.Feature{Meta: domain.Meta{ID: "foreign", ProductID: "q"}, Title: "FOREIGN_FEATURE", Status: "planned"})
			for i := 0; i < 3; i++ {
				state.Integrations = append(state.Integrations, domain.Integration{Meta: domain.Meta{ID: fmt.Sprintf("i%d", i), ProductID: "p", FeatureID: "f000"}, Title: "Integration", Status: "planned"})
			}
			state.Events = append(state.Events, domain.Event{ID: "audit", ProductID: "q", Data: domain.Command{Action: "record_progress", Data: map[string]any{"body": "PRIVATE_AUDIT" + strings.Repeat("a", 8<<20)}}})
			if err := store.Update(ctx, func(s *domain.State) error { *s = state; return nil }); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(transport.New(application.New(store), transport.Options{}))
			defer server.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "bounded-navigation-test", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			call := func(args any, wantError bool) domain.StatePage {
				t.Helper()
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_state", Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				wire, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				if len(wire) > 128<<10 || !utf8.Valid(wire) {
					t.Fatalf("invalid wire budget/UTF8: %d", len(wire))
				}
				for _, marker := range []string{"PRIVATE_CONTEXT", "PRIVATE_AUDIT", "FOREIGN_FEATURE"} {
					if strings.Contains(string(wire), marker) {
						t.Fatalf("unrequested payload leaked: %s", marker)
					}
				}
				if result.IsError != wantError {
					t.Fatalf("error=%v want=%v: %s", result.IsError, wantError, wire)
				}
				if wantError {
					return domain.StatePage{}
				}
				raw, err := json.Marshal(result.StructuredContent)
				if err != nil {
					t.Fatal(err)
				}
				if len(raw) > 32<<10 {
					t.Fatal("projection oversized")
				}
				var page domain.StatePage
				if err = json.Unmarshal(raw, &page); err != nil {
					t.Fatal(err)
				}
				if page.Projection == "" || page.Pagination.Returned != len(page.Items) {
					t.Fatal("missing navigation contract")
				}
				return page
			}
			index := call(map[string]any{"limit": 1}, false)
			if len(index.Items) != 1 || index.Items[0].ID != "p" || !index.Pagination.HasMore {
				t.Fatal("bad Product index")
			}
			next := call(map[string]any{"limit": 1, "cursor": index.Pagination.NextCursor}, false)
			if len(next.Items) != 1 || next.Items[0].ID != "q" || next.Pagination.HasMore {
				t.Fatal("bad Product continuation")
			}
			seen := map[string]bool{}
			cursor := ""
			pages := 0
			for {
				args := map[string]any{"product_id": "p", "limit": 100}
				if cursor != "" {
					args["cursor"] = cursor
				}
				page := call(args, false)
				pages++
				for _, item := range page.Items {
					if seen[item.ID] || item.ProductID != "p" || !item.Truncated || len(item.Title) > 256 || !utf8.ValidString(item.Title) {
						t.Fatal("invalid Feature card")
					}
					seen[item.ID] = true
				}
				if !page.Pagination.HasMore {
					break
				}
				if len(page.Items) == 0 || page.Pagination.NextCursor == cursor {
					t.Fatal("pagination stalled")
				}
				cursor = page.Pagination.NextCursor
				if pages > 120 {
					t.Fatal("pagination loop")
				}
			}
			if len(seen) != 120 || pages < 2 {
				t.Fatalf("lost byte-boundary cards: %d across %d pages", len(seen), pages)
			}
			for _, args := range []any{map[string]any{"feature_id": "f000"}, map[string]any{"product_id": "p", "feature_id": "f000"}} {
				page := call(args, false)
				if len(page.Items) != 3 || page.Scope.Product == nil || page.Scope.Product.ID != "p" || page.Scope.Feature == nil || page.Scope.Feature.ID != "f000" {
					t.Fatal("invalid Feature scope")
				}
			}
			for _, args := range []any{
				map[string]any{"product_id": "q", "feature_id": "f000"}, map[string]any{"feature_id": "missing"}, map[string]any{"product_id": "missing"}, map[string]any{"product_id": " p"}, map[string]any{"product_id": strings.Repeat("x", 129)}, map[string]any{"Product_id": "p"}, map[string]any{"unknown": true}, map[string]any{"limit": 0}, map[string]any{"limit": -1}, map[string]any{"limit": 101}, map[string]any{"limit": 1.5}, map[string]any{"limit": "1"}, map[string]any{"cursor": "bad"}, map[string]any{"cursor": strings.Repeat("a", 2049)}, map[string]any{"product_id": "p", "cursor": index.Pagination.NextCursor}, json.RawMessage(`{"product_id":"p","product_id":"q"}`),
			} {
				call(args, true)
			}
			// The browser/operator snapshot retains full state and its ETag contract.
			response, err := http.Get(server.URL + "/api/state")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || !strings.Contains(string(body), "PRIVATE_AUDIT") || response.Header.Get("ETag") == "" {
				t.Fatal("full HTTP snapshot regressed")
			}
			req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/state", nil)
			req.Header.Set("If-None-Match", response.Header.Get("ETag"))
			unchanged, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			unchanged.Body.Close()
			if unchanged.StatusCode != http.StatusNotModified {
				t.Fatal("HTTP ETag regressed")
			}
		})
	}
}
