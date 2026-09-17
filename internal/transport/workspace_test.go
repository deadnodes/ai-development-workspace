package transport_test

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http/httptest"
	"path/filepath"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
	"testing"
)

func TestEmbeddedWorkspaceMCPEntryAndConfigImport(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "state.db")
	store, e := persistence.OpenLocal(ctx, file)
	if e != nil {
		t.Fatal(e)
	}
	svc := application.New(store)
	if _, e = svc.Execute(ctx, domain.Command{Action: "create_product", Actor: "agent", ID: "p", Data: map[string]any{"name": "Local product"}}); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(transport.New(svc, transport.Options{}))
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	call := func(name string, args any) *mcp.CallToolResult {
		t.Helper()
		r, e := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil || r.IsError {
			raw, _ := json.Marshal(r.Content)
			t.Fatalf("%s: %v %+v content=%s", name, e, r, raw)
		}
		return r
	}
	call("set_project_knowledge", map[string]any{"product_id": "p", "actor": "agent", "knowledge": map[string]any{"overview": "Shared architecture", "instructions": "Check contracts", "areas": []any{}, "relationships": []any{}}})
	call("upsert_product_knowledge", map[string]any{"product_id": "p", "actor": "agent/knowledge", "node": map[string]any{"id": "contract", "kind": "contract", "title": "API contract", "content": "Keep versioned callbacks", "keywords": []string{"api", "callback"}}})
	search := call("search_product_knowledge", map[string]any{"product_id": "p", "query": "callbacks"})
	searchJSON, _ := json.Marshal(search)
	if !json.Valid(searchJSON) || len(searchJSON) == 0 {
		t.Fatal("missing knowledge search result")
	}
	r := call("get_project_context", map[string]any{"product_id": "p"})
	b, _ := json.Marshal(r)
	if len(b) == 0 {
		t.Fatal("missing context")
	}
	call("export_workspace_configuration", map[string]any{"product_id": "p"})
	config, e := svc.ExportWorkspace(ctx, "p")
	if e != nil {
		t.Fatal(e)
	}
	config.Product.ID = "new"
	config.Knowledge.Meta = domain.Meta{}
	call("import_workspace_configuration", map[string]any{"actor": "agent", "configuration": config})
	session.Close()
	srv.Close()
	store.Close()
	reopened, e := persistence.OpenLocal(ctx, file)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	st, e := reopened.Read(ctx)
	if e != nil || len(st.ProjectKnowledge) != 3 || len(st.Products) != 2 || len(st.Features) != 0 {
		t.Fatalf("restart lost knowledge: %v %+v", e, st)
	}
}
