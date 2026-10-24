package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

// Identity injection isolates application/metadata authorization. Credential and
// cookie validation are covered by the existing auth manager boundary tests.
func TestNavigationAuthorizationBeforePagination(t *testing.T) {
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
			state := domain.EmptyState()
			all := []string{"00-hidden", "10-visible", "20-hidden", "30-visible", "40-hidden"}
			for _, id := range all {
				state.Products = append(state.Products, domain.Product{Meta: domain.Meta{ID: id}, Name: id})
			}
			state.Features = []domain.Feature{{Meta: domain.Meta{ID: "visible-feature", ProductID: "10-visible"}, Title: "Visible", Status: "planned"}, {Meta: domain.Meta{ID: "hidden-feature", ProductID: "00-hidden"}, Title: "SECRET_FOREIGN_NAV", Status: "planned"}}
			state.Integrations = []domain.Integration{{Meta: domain.Meta{ID: "visible-integration", ProductID: "10-visible", FeatureID: "visible-feature"}, Title: "Visible", Status: "planned"}, {Meta: domain.Meta{ID: "hidden-integration", ProductID: "00-hidden", FeatureID: "hidden-feature"}, Title: "SECRET_FOREIGN_NAV", Status: "planned"}}
			if e := store.Update(ctx, func(s *domain.State) error { *s = state; return nil }); e != nil {
				t.Fatal(e)
			}
			before, _ := store.Read(ctx)
			beforeJSON, _ := json.Marshal(before)
			for _, tc := range []struct {
				name      string
				principal auth.Principal
				want      []string
				deny      bool
			}{
				{"scoped", auth.Principal{ID: "agent/p", Kind: "agent", Role: "member", Products: []string{"10-visible", "30-visible"}, Permissions: []string{"read"}}, []string{"10-visible", "30-visible"}, false},
				{"empty", auth.Principal{ID: "agent/empty", Kind: "agent", Role: "member", Permissions: []string{"read"}}, []string{}, false},
				{"wildcard", auth.Principal{ID: "agent/all", Kind: "agent", Role: "member", Products: []string{"*"}, Permissions: []string{"read"}}, all, false},
				{"instance admin", auth.Principal{ID: "human/admin", Kind: "human", Role: "admin", Products: []string{"*"}, Permissions: []string{"admin"}}, all, false},
				{"agent admin label", auth.Principal{ID: "agent/labeled", Kind: "agent", Role: "admin", Products: []string{"10-visible"}, Permissions: []string{"read", "admin"}}, []string{"10-visible"}, false},
				{"no read", auth.Principal{ID: "agent/write", Kind: "agent", Role: "member", Products: []string{"10-visible"}, Permissions: []string{"write"}}, nil, true},
				{"wildcard without read", auth.Principal{ID: "agent/no-read", Kind: "agent", Role: "member", Products: []string{"*"}}, nil, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					handler := transport.New(application.New(store), transport.Options{})
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						handler.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), tc.principal)))
					}))
					defer server.Close()
					session, e := mcp.NewClient(&mcp.Implementation{Name: "authorized-navigation", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
					if e != nil {
						t.Fatal(e)
					}
					defer session.Close()
					call := func(args map[string]any, deny bool) domain.StatePage {
						t.Helper()
						result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_state", Arguments: args})
						if e != nil {
							t.Fatal(e)
						}
						if result.IsError != deny {
							t.Fatalf("denied=%v want=%v: %+v", result.IsError, deny, result)
						}
						if deny {
							return domain.StatePage{}
						}
						raw, _ := json.Marshal(result.StructuredContent)
						var page domain.StatePage
						if e = json.Unmarshal(raw, &page); e != nil {
							t.Fatal(e)
						}
						if len(raw) > 32<<10 {
							t.Fatal("oversized projection")
						}
						return page
					}
					if tc.deny {
						call(map[string]any{}, true)
						return
					}
					seen := []string{}
					cursor := ""
					for pageNumber := 0; pageNumber <= len(all); pageNumber++ {
						args := map[string]any{"limit": 1}
						if cursor != "" {
							args["cursor"] = cursor
						}
						page := call(args, false)
						if len(page.Items) > 1 {
							t.Fatal("page overflow")
						}
						for _, card := range page.Items {
							seen = append(seen, card.ID)
						}
						if !page.Pagination.HasMore {
							if page.Pagination.NextCursor != "" {
								t.Fatal("terminal cursor")
							}
							break
						}
						if len(page.Items) != 1 || page.Pagination.NextCursor == cursor {
							t.Fatal("pagination stalled")
						}
						cursor = page.Pagination.NextCursor
					}
					if !reflect.DeepEqual(seen, tc.want) {
						t.Fatalf("scope or page loss: got%v want%v", seen, tc.want)
					}
					if tc.name == "scoped" || tc.name == "agent admin label" {
						page := call(map[string]any{"feature_id": "visible-feature"}, false)
						if len(page.Items) != 1 || page.Items[0].ID != "visible-integration" || page.Scope.Product == nil || page.Scope.Product.ID != "10-visible" {
							t.Fatal("authorized parent resolution")
						}
						call(map[string]any{"product_id": "00-hidden"}, true)
						call(map[string]any{"feature_id": "hidden-feature"}, true)
						call(map[string]any{"product_id": "10-visible", "feature_id": "hidden-feature"}, true)
						call(map[string]any{"allowed_product_ids": []string{"*"}}, true)
					}
				})
			}
			after, _ := store.Read(ctx)
			afterJSON, _ := json.Marshal(after)
			if string(beforeJSON) != string(afterJSON) {
				t.Fatal("navigation mutated state")
			}
		})
	}
}
