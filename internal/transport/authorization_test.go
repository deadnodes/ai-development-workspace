package transport_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
	"releasecontrol/internal/application"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

// Exercise the real SDK tools/call handler with request-scoped identity. Auth
// manager credential validation is tested independently in internal/auth.
func TestScopedHTTPAndMCPUseSameApplicationPolicy(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := application.New(store)
	for _, id := range []string{"p", "private"} {
		_, err = svc.Execute(ctx, domain.Command{Action: "create_product", ID: id, Actor: "setup", Data: map[string]any{"name": id}})
		if err != nil {
			t.Fatal(err)
		}
	}
	principal := auth.Principal{ID: "agent/verified", Kind: "agent", Role: "member", Products: []string{"p"}, Permissions: []string{"read", "write", "deploy"}}
	handler := transport.New(svc, transport.Options{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	defer srv.Close()
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/state", "", 200},
		{"GET", "/api/products/private/workspace-config", "", 403},
		{"POST", "/api/backups/export", "{}", 403},
		{"POST", "/api/workspaces/import", `{"actor":"admin","configuration":{}}`, 403},
		{"POST", "/api/local-git/plan", `{"product_id":"private","checkout_id":"x","actor":"admin","mode":"FETCH"}`, 403},
		{"POST", "/api/local-git/record", `{"product_id":"private","checkout_id":"x","actor":"admin","plan_id":"x"}`, 403},
		{"POST", "/api/reviews/sync", `{"actor":"admin","integration_id":"private-i","repository_id":"private-r","pull_request":1}`, 403},
		{"POST", "/api/commands", `{"action":"grant_connection","product_id":"p","actor":"admin","data":{}}`, 403},
		{"POST", "/api/products/private/knowledge/nodes", `{"actor":"admin","node":{"id":"x","kind":"contract","title":"x","content":"x"}}`, 403},
		{"POST", "/api/products/p/knowledge/nodes", `{"actor":"admin","node":{"id":"x","kind":"contract","title":"x","content":"x"}}`, 200},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, res.StatusCode, body)
		}
		if tc.path == "/api/state" {
			if strings.Contains(string(body), "private") {
				t.Fatal("HTTP state leak")
			}
			if !strings.Contains(res.Header.Get("Vary"), "Cookie") {
				t.Fatal("missing Cookie cache variation")
			}
		}
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "scope-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, tc := range []struct {
		name   string
		args   map[string]any
		denied bool
	}{
		{"create_backup", map[string]any{"actor": "admin"}, true},
		{"restore_backup", map[string]any{"actor": "admin", "archive_base64": "", "sha256": ""}, true},
		{"import_workspace_configuration", map[string]any{"actor": "admin", "configuration": map[string]any{}}, true},
		{"scan_workspace", map[string]any{"actor": "admin", "product_id": "p"}, true},
		{"plan_local_git_sync", map[string]any{"actor": "admin", "product_id": "private", "checkout_id": "x", "mode": "FETCH"}, true},
		{"record_local_git_sync", map[string]any{"actor": "admin", "product_id": "private", "checkout_id": "x", "plan_id": "x"}, true},
		{"sync_pull_request_review", map[string]any{"actor": "admin", "integration_id": "private-i", "repository_id": "private-r", "pull_request": 1}, true},
		{"get_project_context", map[string]any{"product_id": "private"}, true},
		{"export_workspace_configuration", map[string]any{"product_id": "private"}, true},
		{"upsert_product_knowledge", map[string]any{"product_id": "private", "actor": "admin", "node": map[string]any{"id": "n", "kind": "contract", "title": "n", "content": "n"}}, true},
		{"upsert_product_knowledge", map[string]any{"product_id": "p", "actor": "admin", "node": map[string]any{"id": "n", "kind": "contract", "title": "n", "content": "n"}}, false},
		{"get_state", map[string]any{}, false},
	} {
		result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		if e != nil {
			t.Fatal(e)
		}
		if result.IsError != tc.denied {
			t.Fatalf("%s denied=%v: %+v", tc.name, tc.denied, result)
		}
		if tc.name == "get_state" {
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), "private") {
				t.Fatal("MCP state leak")
			}
		}
	}
	state, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range state.ProjectKnowledge {
		if k.Actor != "agent/verified" {
			t.Fatalf("actor spoofed: %s", k.Actor)
		}
	}
}

func TestConfiguredAuthIsMandatoryAndLegacyTokenCannotBypass(t *testing.T) {
	var issuer *httptest.Server
	issuer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize", "token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	defer issuer.Close()
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, issuer.Client())
	manager, err := auth.New(ctx, auth.Config{Mode: "oidc", IssuerURL: issuer.URL, PublicURL: "https://rcp.example", ClientID: "test", AdminRoles: []string{"admin"}, EncryptionKey: []byte(strings.Repeat("k", 32)), FilePath: filepath.Join(t.TempDir(), "auth.db"), BootstrapToken: "test-bootstrap-token-at-least-32-characters"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "domain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := transport.New(application.New(store), transport.Options{Auth: manager, Token: "old-token"})
	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{"/api/state", "", 401}, {"/api/state", "old-token", 401}, {"/mcp", "", 401}, {"/api/state", "test-bootstrap-token-at-least-32-characters", 200}, {"/", "", 200},
	} {
		req := httptest.NewRequest("GET", "http://localhost"+tc.path, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s token=%t: %d %s", tc.path, tc.token != "", rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest("GET", "http://localhost/auth/callback", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", issuer.URL)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_state") {
		t.Fatalf("callback blocked before OIDC state validation: %d %s", rec.Code, rec.Body.String())
	}
	local, err := auth.New(context.Background(), auth.Config{Mode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	localHandler := transport.New(application.New(store), transport.Options{Auth: local, Token: "old-token"})
	req = httptest.NewRequest("GET", "http://localhost/api/state", nil)
	rec = httptest.NewRecorder()
	localHandler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("disabled manager bypassed legacy token")
	}
}
