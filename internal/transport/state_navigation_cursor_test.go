package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

func TestNavigationMaximumIdentityCursorSDK(t *testing.T) {
	for _, backend := range []string{"local", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store application.Store
			if backend == "local" {
				s, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
				if e != nil {
					t.Fatal(e)
				}
				defer s.Close()
				store = s
			} else {
				s, e := persistence.Open(ctx, isolatedDatabase(t))
				if e != nil {
					t.Fatal(e)
				}
				defer s.Close()
				store = s
			}
			product := strings.Repeat("\x01", 128)
			f1, f2 := strings.Repeat("\x02", 128), strings.Repeat("\x03", 128)
			i1, i2 := strings.Repeat("\x04", 128), strings.Repeat("\x05", 128)
			st := domain.EmptyState()
			st.Products = []domain.Product{{Meta: domain.Meta{ID: product}, Name: "Maximum exact identity"}}
			st.Features = []domain.Feature{{Meta: domain.Meta{ID: f1, ProductID: product}, Title: "First", Status: "planned"}, {Meta: domain.Meta{ID: f2, ProductID: product}, Title: "Second", Status: "planned"}}
			st.Integrations = []domain.Integration{{Meta: domain.Meta{ID: i1, ProductID: product, FeatureID: f1}, Title: "First", Status: "planned"}, {Meta: domain.Meta{ID: i2, ProductID: product, FeatureID: f1}, Title: "Second", Status: "planned"}}
			if e := store.Update(ctx, func(s *domain.State) error { *s = st; return nil }); e != nil {
				t.Fatal(e)
			}
			handler := transport.New(application.New(store), transport.Options{})
			principal := auth.Principal{ID: "agent/exact", Kind: "agent", Role: "member", Products: []string{product}, Permissions: []string{"read"}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
			}))
			defer server.Close()
			session, e := mcp.NewClient(&mcp.Implementation{Name: "maximum-cursor", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer session.Close()
			call := func(args map[string]any) domain.StatePage {
				t.Helper()
				res, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_state", Arguments: args})
				if e != nil || res.IsError {
					t.Fatalf("server cursor rejected: %v %+v", e, res)
				}
				raw, _ := json.Marshal(res.StructuredContent)
				var p domain.StatePage
				if e := json.Unmarshal(raw, &p); e != nil {
					t.Fatal(e)
				}
				if len(raw) > 32<<10 {
					t.Fatal("projection overflow")
				}
				wire, _ := json.Marshal(res)
				if len(wire) > 128<<10 {
					t.Fatal("wire overflow")
				}
				return p
			}
			for _, tc := range []struct {
				name          string
				args          map[string]any
				first, second string
			}{
				{"features", map[string]any{"product_id": product, "limit": 1}, f1, f2},
				{"integrations", map[string]any{"product_id": product, "feature_id": f1, "limit": 1}, i1, i2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					first := call(tc.args)
					if len(first.Items) != 1 || first.Items[0].ID != tc.first || !first.Pagination.HasMore || len(first.Pagination.NextCursor) > 2048 {
						t.Fatal("first page identity or cursor limit")
					}
					tc.args["cursor"] = first.Pagination.NextCursor
					last := call(tc.args)
					if len(last.Items) != 1 || last.Items[0].ID != tc.second || last.Pagination.HasMore || last.Pagination.NextCursor != "" {
						t.Fatal("last page identity or termination")
					}
				})
			}
		})
	}
}
